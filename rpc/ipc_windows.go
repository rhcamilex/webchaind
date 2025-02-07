// Copyright 2015 The go-ethereum Authors
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

// +build windows

package rpc

import (
	"net"
	"time"

	winio "github.com/Microsoft/go-winio"
)

// ipcListen will create a named pipe on the given endpoint.
func ipcListen(endpoint string) (net.Listener, error) {
	return winio.ListenPipe(endpoint, &winio.PipeConfig{})
}

// newIPCConnection will connect to a named pipe with the given endpoint as name.
func newIPCConnection(endpoint string) (net.Conn, error) {
	timeout := 5 * time.Second
	return winio.DialPipe(endpoint, &timeout)
}
