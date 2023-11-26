package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	args = 3
	maxRetries = 2
	bufferSize = 1024 * 64
	packetBufferSize = 2
)

/**
	* 00000001 - ACK
	* 00000010 - SYN
	* 00000100 - FIN
	* 00001000 - DATA
**/

const (
	FLAG_ACK = 1 << iota
	FLAG_SYN
	FLAG_FIN
	FLAG_DATA
)

//////////////////define custom packet structure//////////////////////
type CustomPacket struct {
	Header Header  `json:"header"`
	Data string    `json:"data"`
}


type Header struct {
	SeqNum uint32 `json:"seqNum"`
	AckNum uint32  `json:"ackNum"`
	DataLen uint32 `json:"dataLen"`
	Flags byte     `json:"flags"`
}

/////////////////////////define writer FSM///////////////////////////
type WriterState int
type WriterFSM struct {
	err error
	currentState WriterState
	ip net.IP
	port int
	maxRetries int
	udpcon *net.UDPConn
	stdinReader *bufio.Reader
	EOFchan chan struct{} //channel for EOF signal handling
	responseChan chan []byte //channel for response handling
	inputChan chan CustomPacket //channel for input handling
	errorChan chan error //channel for error handling between go routines
	resendChan chan struct{} //channel for resend handling
	stopChan chan struct{} //channel for notifying go routines to stop
	ackByResend chan bool //channel for ack handling by resend
	ack uint32
	seq uint32
	data string
	timeoutDuration time.Duration
	lastPacketMutex sync.Mutex
	lastPacket []byte
	wg sync.WaitGroup
	// packetSent int
	// packetReceived int
}

const (
	ValidateArgs WriterState = iota
	CreateSocket
	SyncronizeServer
	ReadyForTransmitting
	Transmitting
	Recover
	ErrorHandling
	FatalError
	Termination
)

/////////////////////define Methods for WriterFSM for state transitions/////////////////////////
func NewWriterFSM() *WriterFSM {
	return &WriterFSM{
		currentState: ValidateArgs,
		maxRetries: maxRetries,
		stdinReader: bufio.NewReader(os.Stdin),
		responseChan: make(chan []byte),
		inputChan: make(chan CustomPacket, packetBufferSize),
		errorChan: make(chan error),
		resendChan: make(chan struct{}),
		EOFchan: make(chan struct{}),
		stopChan: make(chan struct{}),
		ack: 0,
		seq: 0,
		data: "",
		ackByResend: make(chan bool),
		lastPacket: make([]byte, 0),
		timeoutDuration: 2 * time.Second,
	}
}

func (fsm *WriterFSM) ValidateArgsState() WriterState {
	if (len(os.Args) != args) {

		fsm.err = errors.New("invalid number of arguments, <ip> <port>")
		return FatalError
	}
	fsm.ip, fsm.err = validateIP(os.Args[1])
	if fsm.err != nil {
		return FatalError
	}
	fsm.port, fsm.err = validatePort(os.Args[2])
	if fsm.err != nil {
		return FatalError
	}
	return CreateSocket
}

func (fsm *WriterFSM) CreateSocketState() WriterState {
	addr := &net.UDPAddr{IP: fsm.ip, Port: fsm.port}
	fsm.udpcon, fsm.err = net.DialUDP("udp", nil, addr)
	if fsm.err != nil {
		return FatalError
	}
	return SyncronizeServer
}

func (fsm *WriterFSM) SyncronizeServerState() WriterState {
	fsm.wg.Add(2)

	go fsm.listenResponse()
	go fsm.sendPacket()
	for {
		packet := createPacket(fsm.ack, fsm.seq, FLAG_SYN, "")
		fsm.inputChan <- packet
		select {
			case fsm.err = <- fsm.errorChan:
				return FatalError
			case <- fsm.responseChan:
				return ReadyForTransmitting
			case <- fsm.stopChan:
				return Termination
		}
	}

}

func (fsm *WriterFSM) ReadyForTransmittingState() WriterState {
	fsm.wg.Add(1)
	go fsm.readStdin()
	fmt.Println("Ready for Transmitting")
	select {
		case fsm.err = <- fsm.errorChan:
			return FatalError
		default:
			return Transmitting
	}
}
/////////////////////////////////////////////Transmitting State////////////////////////////////////////

func (fsm *WriterFSM) TransmittingState() WriterState {
	for {
		select {
			case <- fsm.EOFchan:
				return Termination
			case <- fsm.resendChan:
				go fsm.resendPacket()
			case fsm.err = <- fsm.errorChan:
				return ErrorHandling

		}
	}
}

func (fsm *WriterFSM) RecoverState() WriterState {
	fsm.stopChan = make(chan struct{})
	fsm.wg.Add(3)
	go fsm.readStdin()
	go fsm.listenResponse()
	go fsm.sendPacket()
	return Transmitting
}


func (fsm *WriterFSM) ErrorHandlingState() WriterState {
		fmt.Println("Error:", fsm.err)
		close(fsm.stopChan)
		fsm.wg.Wait()
		return ReadyForTransmitting
	}



func (fsm *WriterFSM) FatalErrorState() WriterState {
	fmt.Println("Fatal Error:", fsm.err)
	return Termination
}



func (fsm *WriterFSM)TerminateState() {
	fmt.Println("Termination")
	close(fsm.stopChan)
	fmt.Println("notify all go routines to stop")
	fsm.wg.Wait()
	fsm.udpcon.Close()
	fmt.Println("Client Exiting...")
}

/////////////////////////////run function for WriterFSM////////////////////////////
func (fsm *WriterFSM) Run() {
	for {
		 select{
		 case  err := <-fsm.errorChan:
			  fsm.err = err
			  fsm.currentState = ErrorHandling

		 default:
			switch fsm.currentState {
			case ValidateArgs:
				fsm.currentState = fsm.ValidateArgsState()
			case CreateSocket:
				fsm.currentState = fsm.CreateSocketState()
			case SyncronizeServer:
				fsm.currentState = fsm.SyncronizeServerState()
			case ReadyForTransmitting:
				fsm.currentState = fsm.ReadyForTransmittingState()
			case Transmitting:
				fsm.currentState = fsm.TransmittingState()
			case Recover:
				fsm.currentState = fsm.RecoverState()
			case ErrorHandling:
				fsm.currentState = fsm.ErrorHandlingState()
			case FatalError:
				fsm.currentState = fsm.FatalErrorState()
			case Termination:
				fsm.TerminateState()
				return
			}
		 }
	}
}

/////////////////////////go routines for FSM////////////////////////////
func (fsm *WriterFSM) readStdin() {
	defer fsm.wg.Done()

	for {
		select {
		case <-fsm.stopChan:
			fmt.Println("readStdin got stopChan")
			return
		default:
			readResult := make(chan []byte)
			go func() {
				inputBuffer := make([]byte, bufferSize)
				n, _ := fsm.stdinReader.Read(inputBuffer)
				if n > 0 {
					readResult <- inputBuffer[:n]
				} else {
					close(readResult)
				}
			}()

			select {
			case <-fsm.stopChan:
				fmt.Println("readStdin got stopChan while reading")
				return
			case data, ok := <-readResult:
				if !ok {
					fmt.Println("readStdin EOF or error")
					return
				}
				packet := createPacket(fsm.ack, fsm.seq, FLAG_DATA, string(data))
				fmt.Println("packet created", string(packet.Data))
				fsm.inputChan <- packet
				fsm.seq += uint32(len(data))
			}
		}
	}
}


func (fsm *WriterFSM) sendPacket() {
	defer fsm.wg.Done()
	for {
	select {
		case <- fsm.stopChan:
			fmt.Println("sendPacket get stopChan")
			return
		case rawPacket, ok := <- fsm.inputChan:
			if !ok {
				return
			}
			fmt.Println("get packet from inputChan", string(rawPacket.Data))
			packet, err := json.Marshal(rawPacket)
			if err != nil {
				fsm.errorChan <- err
				return
			}
			_, err = fsm.udpcon.Write(packet)
			fmt.Println("sent packet", string(packet))
			if err != nil {
				fsm.errorChan <- err
				return
			}
			fmt.Println("sent packet", len(packet))
			}
	}
}

func (fsm *WriterFSM) listenResponse() {
	defer fsm.wg.Done()
	for {
		select {
		case <-fsm.stopChan:
			fmt.Println("listenResponse get stopChan")
			return
		default:
			fsm.udpcon.SetReadDeadline(time.Now().Add(fsm.timeoutDuration))
			buffer := make([]byte, bufferSize)
			n, err := fsm.udpcon.Read(buffer)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue 
				}
				 if netErr, ok := err.(net.Error);ok && net.ErrClosed == netErr {
					fmt.Println("connection closed")
					fsm.errorChan <- err
					return
				}
				if netErr, ok := err.(net.Error); ok && strings.Contains(netErr.Error(), "connection refused"){
					fmt.Println("connection refused")
					fsm.errorChan <- err
					return

				}
				fsm.errorChan <- err
				return
			}

			fsm.responseChan <- buffer[:n]
		}
	}
}


func (fsm *WriterFSM) resendPacket() {
	for i := 0; i < fsm.maxRetries; i++ {
		select {
		case <-fsm.stopChan:
			fmt.Println("resendPacket get stopChan")
			return
		default:
			fsm.lastPacketMutex.Lock()
			packet := make([]byte, len(fsm.lastPacket))
			copy(packet, fsm.lastPacket)
			fsm.lastPacketMutex.Unlock()
			_, err := fsm.udpcon.Write(packet)
			if err != nil {
				select {
				case fsm.errorChan <- err:
				case <-fsm.stopChan:
					return
				}
			}
			select {
			case <-fsm.stopChan:
				return
			case response := <-fsm.responseChan:
				if validPacket(response, FLAG_ACK, fsm.seq) {
					return 
				}
			case <-time.After(fsm.timeoutDuration):
			}
		}
	}
	fsm.errorChan <- fmt.Errorf("max retries exceeded for packet: %v", fsm.lastPacket)
}



////////////////////////////////helper functions///////////////////////////////
func validateIP(ip string) (net.IP, error){
	addr := net.ParseIP(ip)
	if addr == nil {
		return nil, errors.New("invalid ip address")
	}
	return addr, nil
}

func validatePort(port string) (int, error) {
	portNo, err := strconv.Atoi(port)
	if err != nil || portNo < 0 || portNo > 65535 {
		return -1, errors.New("invalid port number")
	}
	return portNo, nil
}

func createPacket(ack uint32, seq uint32, flags byte, data string) CustomPacket {
	packet := CustomPacket{
		Header: Header{
			SeqNum: seq,
			AckNum: ack,
			DataLen: uint32(len(data)),
			Flags: flags,
		},
		Data: data,

	}
	return packet

}


func validPacket(response []byte, flags byte, seq uint32) bool {
	header,  err := parsePacket(response)
	if err != nil {
		return false
	}
	fmt.Println("expected ackNum: ", fmt.Sprint(seq))
	fmt.Println("actual ackNum: ", fmt.Sprint(header.AckNum))
	return header.Flags == flags && header.AckNum == seq
}

func parsePacket(response []byte) (*Header,  error) {
	var packet CustomPacket
	err := json.Unmarshal(response, &packet)
	if err != nil {
		return nil,  err
	}
	return &packet.Header, nil
}

/////////////////////////////main function//////////////////////////////////////

func main() {
	writerFSM := NewWriterFSM()
	writerFSM.Run()
}
