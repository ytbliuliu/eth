package main

//@NOTE SHYFT main func for api, sets up router and spins up a server
//to run server 'go run shyftRingWalletConn/*.go'
import (
	"bufio"
	"fmt"
	"github.com/ShyftNetwork/go-empyrean/common/hexutil"
	"github.com/ShyftNetwork/go-empyrean/crypto"
	"io"
	"net"
	"net/http"
	"os"
	"github.com/ShyftNetwork/go-empyrean/ethclient"
	"github.com/ShyftNetwork/go-empyrean/common"
	"context"
	"time"
	"sync"
)

const (
	CONN_HOST     = "localhost"
	CONN_PORT     = "3333"
	CONN_TYPE     = "tcp"
	NEW_LINE_BYTE = 0x0a
)

var testAddrHex = "14791697260E4c9A71f18484C9f997B308e59325"
var testPrivHex = "0123456789012345678901234567890123456789012345678901234567890123"

var client = &http.Client{}

var mutex = &sync.Mutex{}

// This gives context to the signed message and prevents signing of transactions.
func signHash(data []byte) []byte {
	msg := fmt.Sprintf("\x19Ethereum Signed Message:\n%d%s", len(data), data)
	return crypto.Keccak256([]byte(msg))
}

func main() {
	l, err := net.Listen(CONN_TYPE, CONN_HOST+":"+CONN_PORT)
	if err != nil {
		fmt.Println("Error listening:", err.Error())
		os.Exit(1)
	}
	defer l.Close()

	for {
		// Listen for an incoming connection.
		conn, err := l.Accept()
		if err != nil {
			fmt.Println("Error accepting: ", err.Error())
			os.Exit(1)
		}
		// Handle connections in a new goroutine.
		go handleRequest(conn)
	}
}

// Handles incoming requests.
func handleRequest(conn net.Conn) {

	messages := make(chan []byte)
	checkBalanceChan := make(chan []byte)

	go readerConn(conn, messages)
	go handleMessages(messages, checkBalanceChan)
	go checkBalance(checkBalanceChan, conn)

	sendRingSignedMsg(conn)
}

func handleMessages(channel chan []byte, checkBalancesChan chan []byte) {
	var prevMsg []byte
	var addressOfClient []byte
	var signatureFromClient []byte
	var msgFromClient []byte

	for {
		msg := <-channel

		//similar to shift in bash
		if prevMsg != nil {
			s := string(prevMsg[:])
			if s == "-- ADDRESS --" {
				fmt.Println("putting on channel 1")
				addressOfClient = msg
				checkBalancesChan <- addressOfClient
			}
			if s == "-- GET_BALANCE --" {
				fmt.Println("putting on channel 3")
				checkBalancesChan <- msg
			}
			if s == "-- SIGNATURE --" {
				signatureFromClient = msg
			}
			if s == "-- MESSAGE --" {
				msgFromClient = msg
			}
			prevMsg = nil
		} else {
			prevMsg = msg
		}

		if addressOfClient != nil && signatureFromClient != nil && msgFromClient != nil {
			sig := string(signatureFromClient[:])
			var sigByteArr, error = hexutil.Decode(sig)

			if error != nil {
				fmt.Println(error)
			}

			var sigHex = hexutil.Bytes(sigByteArr)
			sigHex[64] -= 27

			signedMsgHash := signHash(msgFromClient)

			var rpk, err = crypto.Ecrecover(signedMsgHash, sigHex)
			if err != nil {
				fmt.Println(err)
			}

			pubKey := crypto.ToECDSAPub(rpk)
			recoveredAddr := crypto.PubkeyToAddress(*pubKey)
			fmt.Println("ADDRESS IS ::", recoveredAddr.Hex())
			signatureFromClient = nil
			msgFromClient = nil
		}
	}
}

func readerConn(conn net.Conn, channel chan []byte) {
	bufReader := bufio.NewReader(conn)

	for {
		msg, err := bufReader.ReadBytes(NEW_LINE_BYTE)

		if err == io.EOF {
			fmt.Println("END OF FILE, CLOSING CONNECTION")
			conn.Close()
			conn = nil
			break
		}
		if err != nil {
			fmt.Println("Connection error: ", err)
			break
		}

		msg = msg[:len(msg)-1] // remove trailing new line byte

		channel <- msg
	}
}

func checkBalance(checkBalanceChan chan []byte, conn net.Conn) {
	fmt.Println("in check balance function")
	c, err := ethclient.Dial("http://127.0.0.1:8545")
	if err != nil {
		fmt.Println("Eth Client not initialized: " , err)
	}

	for {
		address := <-checkBalanceChan
		fmt.Println("the address is ", string(address[:]))


		balance, error := c.BalanceAt(context.Background(), common.HexToAddress(string(address[:])),nil)
		if error != nil {
			fmt.Println("Balance at error ", error)
		}
		mutex.Lock()
		fmt.Println("in broadcasting balance")
		fmt.Println("the bal is ", balance)
		fmt.Println("The balance for address ", string(address[:]), " is ", balance)
		fmt.Println([]byte("Broadcasting Balance"))
		fmt.Println([]byte("\n"))
		fmt.Println([]byte(balance.String()))
		fmt.Println([]byte("\n"))
		conn.Write([]byte("Broadcasting Balance"))
		conn.Write([]byte("\n"))
		conn.Write([]byte(balance.String()))
		conn.Write([]byte("\n"))
		mutex.Unlock()
	}
}

func sendRingSignedMsg(conn net.Conn){
	key, _ := crypto.HexToECDSA(testPrivHex)

	f_msg := "Hello World"
	first_message := []byte(f_msg)
	new_msg2 := crypto.Keccak256(first_message)

	//send_message := append(new_msg2, []byte{byte(10)}...)
	new_sig, err := crypto.Sign(new_msg2, key)
	if err != nil {
		fmt.Println("The crypto.Sign err is ", err)
	}

	mutex.Lock()
	fmt.Println("in broadcasting message")
	fmt.Println([]byte("Broadcasting Message"))
	fmt.Println([]byte("\n"))
	fmt.Println([]byte(f_msg))
	fmt.Println([]byte("\n"))
	fmt.Println(new_sig)
	fmt.Println([]byte("\n"))
	conn.Write([]byte("Broadcasting Message"))
	time.Sleep(1000 * time.Millisecond)
	conn.Write([]byte("\n"))
	time.Sleep(3000 * time.Millisecond)
	conn.Write([]byte(f_msg))
	time.Sleep(1000 * time.Millisecond)
	conn.Write([]byte("\n"))
	conn.Write(new_sig)
	time.Sleep(1000 * time.Millisecond)
	conn.Write([]byte("\n"))
	mutex.Unlock()
}