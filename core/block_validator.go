// Copyright 2015 The go-ethereum Authors
// Copyright 2018 Webchain project
// This file is part of Webchain.
//
// Webchain is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Webchain is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Webchain. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"fmt"
	"math/big"
	"time"

	"github.com/webchain-network/webchaind/common"
	"github.com/webchain-network/webchaind/core/state"
	"github.com/webchain-network/webchaind/core/types"
	"github.com/webchain-network/webchaind/logger/glog"
	"github.com/webchain-network/webchaind/params"
	"github.com/webchain-network/webchaind/pow"
	"gopkg.in/fatih/set.v0"
)

var (
	MinGasLimit            = big.NewInt(5000)    // Minimum the gas limit may ever be.
	TargetGasLimit         = big.NewInt(4712388) // The artificial target
	GasLimitBoundDivisor   = big.NewInt(1024)    // The bound divisor of the gas limit, used in update calculations.
)

var (
	big1       = big.NewInt(1)
	big2       = big.NewInt(2)
	big9       = big.NewInt(9)
	big10      = big.NewInt(10)
	bigMinus20 = big.NewInt(-20)
)

// Difficulty allows passing configurable options to a given difficulty algorithm.
type DifficultyConfig struct {
	Name    string                 `json:"name"`
	Options map[string]interface{} `json:"options"`
}

// BlockValidator is responsible for validating block headers, uncles and
// processed state.
//
// BlockValidator implements Validator.
type BlockValidator struct {
	config *ChainConfig // Chain configuration options
	bc     *BlockChain  // Canonical block chain
	Pow    pow.PoW      // Proof of work used for validating
}

// NewBlockValidator returns a new block validator which is safe for re-use
func NewBlockValidator(config *ChainConfig, blockchain *BlockChain, pow pow.PoW) *BlockValidator {
	validator := &BlockValidator{
		config: config,
		Pow:    pow,
		bc:     blockchain,
	}
	return validator
}

// ValidateBlock validates the given block's header and uncles and verifies the
// the block header's transaction and uncle roots.
//
// ValidateBlock does not validate the header's pow. The pow work validated
// separately so we can process them in parallel.
//
// ValidateBlock also validates and makes sure that any previous state (or present)
// state that might or might not be present is checked to make sure that fast
// sync has done it's job proper. This prevents the block validator form accepting
// false positives where a header is present but the state is not.
func (v *BlockValidator) ValidateBlock(block *types.Block) error {
	if v.bc.HasBlock(block.Hash()) {
		if _, err := state.New(block.Root(), state.NewDatabase(v.bc.chainDb)); err == nil {
			return &KnownBlockError{block.Number(), block.Hash()}
		}
	}
	parent := v.bc.GetBlock(block.ParentHash())
	if parent == nil {
		return ParentError(block.ParentHash())
	}
	if _, err := state.New(parent.Root(), state.NewDatabase(v.bc.chainDb)); err != nil {
		return ParentError(block.ParentHash())
	}

	header := block.Header()
	// validate the block header
	if err := ValidateHeader(v.config, v.Pow, header, parent.Header(), false, false); err != nil {
		return err
	}
	// verify the uncles are correctly rewarded
	if err := v.VerifyUncles(block, parent); err != nil {
		return err
	}

	// Verify UncleHash before running other uncle validations
	unclesSha := types.CalcUncleHash(block.Uncles())
	if unclesSha != header.UncleHash {
		return fmt.Errorf("invalid uncles root hash. received=%x calculated=%x", header.UncleHash, unclesSha)
	}

	// The transactions Trie's root (R = (Tr [[i, RLP(T1)], [i, RLP(T2)], ... [n, RLP(Tn)]]))
	// can be used by light clients to make sure they've received the correct Txs
	txSha := types.DeriveSha(block.Transactions())
	if txSha != header.TxHash {
		return fmt.Errorf("invalid transaction root hash. received=%x calculated=%x", header.TxHash, txSha)
	}

	return nil
}

// ValidateState validates the various changes that happen after a state
// transition, such as amount of used gas, the receipt roots and the state root
// itself. ValidateState returns a database batch if the validation was a success
// otherwise nil and an error is returned.
func (v *BlockValidator) ValidateState(block, parent *types.Block, statedb *state.StateDB, receipts types.Receipts, usedGas *big.Int) (err error) {
	header := block.Header()
	if block.GasUsed().Cmp(usedGas) != 0 {
		return validateError(fmt.Sprintf("gas used error (%v / %v)", block.GasUsed(), usedGas))
	}
	// Validate the received block's bloom with the one derived from the generated receipts.
	// For valid blocks this should always validate to true.
	rbloom := types.CreateBloom(receipts)
	if rbloom != header.Bloom {
		return fmt.Errorf("unable to replicate block's bloom=%x vs calculated bloom=%x", header.Bloom, rbloom)
	}
	// Tre receipt Trie's root (R = (Tr [[H1, R1], ... [Hn, R1]]))
	receiptSha := types.DeriveSha(receipts)
	if receiptSha != header.ReceiptHash {
		return fmt.Errorf("invalid receipt root hash. received=%x calculated=%x", header.ReceiptHash, receiptSha)
	}
	// Validate the state root against the received state root and throw
	// an error if they don't match.
	if root := statedb.IntermediateRoot(v.config.IsAtlantis(header.Number)); header.Root != root {
		return fmt.Errorf("invalid merkle root: header=%x computed=%x", header.Root, root)
	}
	return nil
}

// VerifyUncles verifies the given block's uncles and applies the Ethereum
// consensus rules to the various block headers included; it will return an
// error if any of the included uncle headers were invalid. It returns an error
// if the validation failed.
func (v *BlockValidator) VerifyUncles(block, parent *types.Block) error {
	// validate that there at most 2 uncles included in this block
	if len(block.Uncles()) > 2 {
		return validateError(fmt.Sprintf("Block can only contain maximum 2 uncles (contained %d)", len(block.Uncles())))
	}

	uncles := set.New()
	ancestors := make(map[common.Hash]*types.Block)
	for _, ancestor := range v.bc.GetBlocksFromHash(block.ParentHash(), 7) {
		ancestors[ancestor.Hash()] = ancestor
		// Include ancestors uncles in the uncle set. Uncles must be unique.
		for _, uncle := range ancestor.Uncles() {
			uncles.Add(uncle.Hash())
		}
	}
	ancestors[block.Hash()] = block
	uncles.Add(block.Hash())

	for i, uncle := range block.Uncles() {
		hash := uncle.Hash()
		if uncles.Has(hash) {
			// Error not unique
			return UncleError("uncle[%d](%x) not unique", i, hash[:4])
		}
		uncles.Add(hash)

		if ancestors[hash] != nil {
			branch := fmt.Sprintf("  O - %x\n  |\n", block.Hash())
			for h := range ancestors {
				branch += fmt.Sprintf("  O - %x\n  |\n", h)
			}
			glog.Infoln(branch)
			return UncleError("uncle[%d](%x) is ancestor", i, hash[:4])
		}

		if ancestors[uncle.ParentHash] == nil || uncle.ParentHash == parent.Hash() {
			return UncleError("uncle[%d](%x)'s parent is not ancestor (%x)", i, hash[:4], uncle.ParentHash[0:4])
		}

		if err := ValidateHeader(v.config, v.Pow, uncle, ancestors[uncle.ParentHash].Header(), true, true); err != nil {
			return validateError(fmt.Sprintf("uncle[%d](%x) header invalid: %v", i, hash[:4], err))
		}
	}

	return nil
}

// ValidateHeader validates the given header and, depending on the pow arg,
// checks the proof of work of the given header. Returns an error if the
// validation failed.
func (v *BlockValidator) ValidateHeader(header, parent *types.Header, checkPow bool) error {
	// Short circuit if the parent is missing.
	if parent == nil {
		return ParentError(header.ParentHash)
	}
	// Short circuit if the header's already known or its parent missing
	if v.bc.HasHeader(header.Hash()) {
		return nil
	}
	return ValidateHeader(v.config, v.Pow, header, parent, checkPow, false)
}

// Validates a header. Returns an error if the header is invalid.
//
// See YP section 4.3.4. "Block Header Validity"
func ValidateHeader(config *ChainConfig, pow pow.PoW, header *types.Header, parent *types.Header, checkPow, uncle bool) error {
	if header.Number.Cmp(big.NewInt(int64(config.GetStopBlock()))) > 0 {
		return fmt.Errorf("block number %d exceeds Stop block (%d)", header.Number.Uint64(), config.GetStopBlock())
	}

	if len(header.Extra) > types.HeaderExtraMax {
		return fmt.Errorf("extra data size %d exceeds limit of %d", len(header.Extra), types.HeaderExtraMax)
	}

	if uncle {
		if header.Time.Cmp(common.MaxBig) == 1 {
			return BlockTSTooBigErr
		}
	} else {
		if header.Time.Cmp(big.NewInt(time.Now().Unix())) == 1 {
			return BlockFutureErr
		}
	}
	if header.Time.Cmp(parent.Time) != 1 {
		return BlockEqualTSErr
	}

	expd := CalcDifficulty(config, header.Time.Uint64(), parent)
	if expd.Cmp(header.Difficulty) != 0 {
		return fmt.Errorf("Difficulty check failed for header %v != %v at %v", header.Difficulty, expd, header.Number)
	}

	a := new(big.Int).Set(parent.GasLimit)
	a = a.Sub(a, header.GasLimit)
	a.Abs(a)
	b := new(big.Int).Set(parent.GasLimit)
	b = b.Div(b, GasLimitBoundDivisor)
	if !(a.Cmp(b) < 0) || (header.GasLimit.Cmp(MinGasLimit) == -1) {
		return fmt.Errorf("GasLimit check failed for header %v (%v > %v)", header.GasLimit, a, b)
	}

	num := new(big.Int).Set(parent.Number)
	num.Sub(header.Number, num)
	if num.Cmp(big.NewInt(1)) != 0 {
		return BlockNumberErr
	}

	if checkPow {
		// Verify the nonce of the header. Return an error if it's not valid
		if !pow.Verify(types.NewBlockWithHeader(header)) {
			return &BlockNonceErr{header.Number, header.Hash(), header.Nonce.Uint64()}
		}
	}
	// If all checks passed, validate the extra-data field for hard forks
	return nil
}

// CalcDifficulty is the difficulty adjustment algorithm. It returns
// the difficulty that a new block should have when created at time
// given the parent block's time and difficulty.
func CalcDifficulty(config *ChainConfig, time uint64, parent *types.Header) *big.Int {
	parentTime := parent.Time.Uint64()
	parentNumber := parent.Number
	parentDiff := parent.Difficulty

	if config == nil {
		glog.Fatalln("missing chain configuration, cannot calculate difficulty")
	}
	if parentDiff == nil {
		parentDiff = big.NewInt(0)
	}
	num := new(big.Int).Add(parentNumber, common.Big1) // increment block number to current

	f, _, configured := config.GetFeature(num, "difficulty")
	if !configured {
		return calcDifficultyDefused(time, parentTime, parentNumber, parentDiff)
	}
	name, ok := f.GetString("type")
	if !ok {
		name = ""
	} // will fall to default panic
	switch name {
	case "atlantis":
		return calcDifficultyAtlantis(time, parent)
	case "defused":
		return calcDifficultyDefused(time, parentTime, parentNumber, parentDiff)
	default:
		panic(fmt.Sprintf("Unsupported difficulty '%v' for block: %v", name, num))
	}
}

func calcDifficultyAtlantis(time uint64, parent *types.Header) *big.Int {
	// https://github.com/ethereum/EIPs/issues/100.
	// algorithm:
	// diff = (parent_diff +
	//         (parent_diff / 200 * max((2 if len(parent.uncles) else 1) - ((timestamp - parent.timestamp) // 9), -20))
	//        ) + 2^(periodCount - 2)

	bigTime := new(big.Int).SetUint64(time)
	bigParentTime := parent.Time

	// holds intermediate values to make the algo easier to read & audit
	x := new(big.Int)
	y := new(big.Int)

	// (2 if len(parent_uncles) else 1) - (block_timestamp - parent_timestamp) // 9
	x.Sub(bigTime, bigParentTime)
	x.Div(x, big9)
	if parent.UncleHash == types.EmptyUncleHash {
		x.Sub(big1, x)
	} else {
		x.Sub(big2, x)
	}
	// max((2 if len(parent_uncles) else 1) - (block_timestamp - parent_timestamp) // 9, -20)
	if x.Cmp(bigMinus20) < 0 {
		x.Set(bigMinus20)
	}
	// parent_diff + (parent_diff / 200 * max((2 if len(parent.uncles) else 1) - ((timestamp - parent.timestamp) // 9), -20))
	y.Div(parent.Difficulty, params.DifficultyBoundDivisor)
	x.Mul(y, x)
	x.Add(parent.Difficulty, x)

	// minimum difficulty can ever be (before exponential factor)
	if x.Cmp(params.MinimumDifficulty) < 0 {
		x.Set(params.MinimumDifficulty)
	}
	return x
}

func calcDifficultyDefused(time, parentTime uint64, parentNumber, parentDiff *big.Int) *big.Int {
	// https://github.com/ethereum/EIPs/blob/master/EIPS/eip-2.mediawiki
	// algorithm:
	// diff = (parent_diff +
	//         (parent_diff / 200 * max(1 - (block_timestamp - parent_timestamp) // 10, -20))
	//        )

	bigTime := new(big.Int).SetUint64(time)
	bigParentTime := new(big.Int).SetUint64(parentTime)

	// holds intermediate values to make the algo easier to read & audit
	x := new(big.Int)
	y := new(big.Int)

	// 1 - (block_timestamp -parent_timestamp) // 10
	x.Sub(bigTime, bigParentTime)
	x.Div(x, big10)
	x.Sub(common.Big1, x)

	// max(1 - (block_timestamp - parent_timestamp) // 10, -20)))
	if x.Cmp(bigMinus20) < 0 {
		x.Set(bigMinus20)
	}

	// (parent_diff + parent_diff // 200 * max(1 - (block_timestamp - parent_timestamp) // 10, -20))
	y.Div(parentDiff, params.DifficultyBoundDivisor)
	x.Mul(y, x)
	x.Add(parentDiff, x)

	// minimum difficulty can ever be (before exponential factor)
	if x.Cmp(params.MinimumDifficulty) < 0 {
		x.Set(params.MinimumDifficulty)
	}

	return x
}

// CalcGasLimit computes the gas limit of the next block after parent.
// The result may be modified by the caller.
// This is miner strategy, not consensus protocol.
func CalcGasLimit(parent *types.Block) *big.Int {
	// contrib = (parentGasUsed * 3 / 2) / 1024
	contrib := new(big.Int).Mul(parent.GasUsed(), big.NewInt(3))
	contrib = contrib.Div(contrib, big.NewInt(2))
	contrib = contrib.Div(contrib, GasLimitBoundDivisor)

	// decay = parentGasLimit / 1024 -1
	decay := new(big.Int).Div(parent.GasLimit(), GasLimitBoundDivisor)
	decay.Sub(decay, big.NewInt(1))

	/*
		strategy: gasLimit of block-to-mine is set based on parent's
		gasUsed value.  if parentGasUsed > parentGasLimit * (2/3) then we
		increase it, otherwise lower it (or leave it unchanged if it's right
		at that usage) the amount increased/decreased depends on how far away
		from parentGasLimit * (2/3) parentGasUsed is.
	*/
	gl := new(big.Int).Sub(parent.GasLimit(), decay)
	gl = gl.Add(gl, contrib)
	gl.Set(common.BigMax(gl, MinGasLimit))

	// however, if we're now below the target (TargetGasLimit) we increase the
	// limit as much as we can (parentGasLimit / 1024 -1)
	if gl.Cmp(TargetGasLimit) < 0 {
		gl.Add(parent.GasLimit(), decay)
		gl.Set(common.BigMin(gl, TargetGasLimit))
	}
	return gl
}
