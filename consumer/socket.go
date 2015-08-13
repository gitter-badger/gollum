// Copyright 2015 trivago GmbH
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package consumer

import (
	"container/list"
	"fmt"
	"github.com/trivago/gollum/core"
	"github.com/trivago/gollum/core/log"
	"github.com/trivago/gollum/shared"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	socketBufferGrowSize = 256
)

// Socket consumer plugin
// Configuration example
//
//   - "consumer.Socket":
//     Enable: true
//     Address: ":5880"
//     Permissions: "0770"
//     Acknowledge: ""
//     Partitioner: "delimiter"
//     Delimiter: "\n"
//     Offset: 0
//     Size: 1
//     ReconnectAfterSec: 2
//     Stream:
//       - "socket"
//
// The socket consumer reads messages directly as-is from a given socket.
// Messages are separated from the stream by using a specific paritioner method.
//
// Address stores the identifier to bind to.
// This can either be any ip address and port like "localhost:5880" or a file
// like "unix:///var/gollum.socket". By default this is set to ":5880".
//
// Permissions sets the file permissions for "unix://" based connections as an
// four digit octal number string. By default this is set to "0770".
//
// Acknowledge can be set to a non-empty value to inform the writer on success
// or error. On success the given string is send. Any error will close the
// connection. This setting is disabled by default, i.e. set to "".
// If Acknowledge is enabled and a IP-Address is given to Address, TCP is
// used to open the connection, otherwise UDP is used.
// If an error occurs during write "NOT <Acknowledge>" is returned.
//
// Partitioner defines the algorithm used to read messages from the stream.
// By default this is set to "delimiter".
//  - "delimiter" separates messages by looking for a delimiter string. The
//    delimiter is removed from the message.
//  - "ascii" reads an ASCII encoded number at a given offset until a given
//    delimiter is found. Everything to the right of and including the delimiter
//    is removed from the message.
//  - "binary" reads a binary number at a given offset and size
//  - "binary_le" is an alias for "binary"
//  - "binary_be" is the same as "binary" but uses big endian encoding
//  - "fixed" assumes fixed size messages
//
// Delimiter defines the delimiter used by the text and delimiter partitioner.
// By default this is set to "\n".
//
// Offset defines the offset used by the binary and text paritioner.
// By default this is set to 0. This setting is ignored by the fixed partitioner.
//
// Size defines the size in bytes used by the binary or fixed partitioner.
// For binary this can be set to 1,2,4 or 8. By default 4 is chosen.
// For fixed this defines the size of a message. By default 1 is chosen.
//
// ReconnectAfterSec defines the number of seconds to wait before a connection
// is tried to be reopened again. By default this is set to 2.
type Socket struct {
	core.ConsumerBase
	listen        io.Closer
	clientLock    *sync.Mutex
	clients       *list.List
	protocol      string
	address       string
	delimiter     string
	acknowledge   string
	reconnectTime time.Duration
	flags         shared.BufferedReaderFlags
	fileFlags     os.FileMode
	offset        int
}

func init() {
	shared.TypeRegistry.Register(Socket{})
}

// Configure initializes this consumer with values from a plugin config.
func (cons *Socket) Configure(conf core.PluginConfig) error {
	err := cons.ConsumerBase.Configure(conf)
	if err != nil {
		return err
	}

	flags, err := strconv.ParseInt(conf.GetString("Permissions", "0770"), 8, 32)
	cons.fileFlags = os.FileMode(flags)
	if err != nil {
		return err
	}

	cons.clients = list.New()
	cons.clientLock = new(sync.Mutex)
	cons.acknowledge = shared.Unescape(conf.GetString("Acknowledge", ""))
	cons.address, cons.protocol = shared.ParseAddress(conf.GetString("Address", ":5880"))
	cons.reconnectTime = time.Duration(conf.GetInt("ReconnectAfterSec", 2)) * time.Second

	if cons.protocol != "unix" {
		if cons.acknowledge != "" {
			cons.protocol = "tcp"
		} else {
			cons.protocol = "udp"
		}
	}

	cons.delimiter = shared.Unescape(conf.GetString("Delimiter", "\n"))
	cons.offset = conf.GetInt("Offset", 0)
	cons.flags = 0

	partitioner := strings.ToLower(conf.GetString("Partitioner", "delimiter"))
	switch partitioner {
	case "binary_be":
		cons.flags |= shared.BufferedReaderFlagBigEndian
		fallthrough

	case "binary", "binary_le":
		cons.flags |= shared.BufferedReaderFlagEverything
		switch conf.GetInt("Size", 4) {
		case 1:
			cons.flags |= shared.BufferedReaderFlagMLE8
		case 2:
			cons.flags |= shared.BufferedReaderFlagMLE16
		case 4:
			cons.flags |= shared.BufferedReaderFlagMLE32
		case 8:
			cons.flags |= shared.BufferedReaderFlagMLE64
		default:
			return fmt.Errorf("Size only supports the value 1,2,4 and 8")
		}

	case "fixed":
		cons.flags |= shared.BufferedReaderFlagMLEFixed
		cons.offset = conf.GetInt("Size", 1)

	case "ascii":
		cons.flags |= shared.BufferedReaderFlagMLE

	case "delimiter":
		// Nothing to add

	default:
		return fmt.Errorf("Unknown partitioner: %s", partitioner)
	}

	return err
}

func (cons *Socket) processConnection(conn net.Conn) {
	cons.AddWorker()
	defer cons.WorkerDone()
	defer conn.Close()

	conn.SetDeadline(time.Time{})
	buffer := shared.NewBufferedReader(socketBufferGrowSize, cons.flags, cons.offset, cons.delimiter)

	for cons.IsActive() {
		err := buffer.ReadAll(conn, cons.Enqueue)

		// Handle errors
		if err != nil {
			// Silently exit on disconnect/close
			if !cons.IsActive() || shared.IsDisconnectedError(err) {
				return // ### return, connection closed ###
			}

			Log.Error.Print("Socket read failed: ", err)
			cons.sendAck(conn, false)
			return // ### return, close connections ###
		}

		// Send ack if everything was ok
		cons.sendAck(conn, true)
	}
}

func (cons *Socket) processClientConnection(clientElement *list.Element) {
	defer func() {
		cons.clientLock.Lock()
		cons.clients.Remove(clientElement)
		cons.clientLock.Unlock()
	}()

	conn := clientElement.Value.(net.Conn)
	cons.processConnection(conn)
}

func (cons *Socket) udpAccept() {
	defer cons.WorkerDone()
	defer cons.closeConnection()
	addr, _ := net.ResolveUDPAddr(cons.protocol, cons.address)

	for cons.IsActive() {
		// (re)open a tcp connection
		for cons.listen == nil {
			if listener, err := net.ListenUDP(cons.protocol, addr); err == nil {
				cons.listen = listener
			} else {
				Log.Error.Print("Socket connection error: ", err)
				time.Sleep(cons.reconnectTime)
			}
		}

		conn := cons.listen.(*net.UDPConn)
		cons.processConnection(conn)
		cons.listen = nil
	}
}

func (cons *Socket) tcpAccept() {
	defer cons.WorkerDone()
	defer cons.closeTCPConnection()

	for cons.IsActive() {
		// (re)open a tcp connection
		for cons.listen == nil {
			if listener, err := net.Listen(cons.protocol, cons.address); err == nil {
				cons.listen = listener
				if cons.protocol == "unix" {
					os.Chmod(cons.address, cons.fileFlags)
				}
			} else {
				Log.Error.Print("Socket connection error: ", err)
				time.Sleep(cons.reconnectTime)
			}
		}

		listener := cons.listen.(net.Listener)
		if client, err := listener.Accept(); err != nil {
			// Trigger full reconnect (suppress errors during shutdown)
			if cons.IsActive() {
				Log.Error.Print("Socket accept failed: ", err)
			}
			cons.closeTCPConnection()
		} else {
			// Handle client connection
			cons.clientLock.Lock()
			element := cons.clients.PushBack(client)
			cons.clientLock.Unlock()
			go cons.processClientConnection(element)
		}
	}
}

func (cons *Socket) sendAck(conn net.Conn, success bool) {
	if cons.acknowledge != "" {
		if success {
			fmt.Fprint(conn, cons.acknowledge)
		} else {
			fmt.Fprint(conn, "NOT "+cons.acknowledge)
		}
	}
}

func (cons *Socket) closeConnection() {
	if cons.listen != nil {
		cons.listen.Close()
		cons.listen = nil
	}
}

func (cons *Socket) closeTCPConnection() {
	cons.closeConnection()
	cons.closeAllClients()
}

func (cons *Socket) closeAllClients() {
	cons.clientLock.Lock()
	defer cons.clientLock.Unlock()

	for cons.clients.Len() > 0 {
		client := cons.clients.Front()
		conn := client.Value.(net.Conn)
		cons.clients.Remove(client)
		conn.Close()
	}
}

// Consume listens to a given socket.
func (cons *Socket) Consume(workers *sync.WaitGroup) {
	cons.AddMainWorker(workers)

	if cons.protocol == "udp" {
		go shared.DontPanic(cons.udpAccept)
		defer cons.closeConnection()
	} else {
		go shared.DontPanic(cons.tcpAccept)
		defer cons.closeTCPConnection()
	}

	cons.ControlLoop()
}
