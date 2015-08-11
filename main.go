package main

import (
	"bufio"
	"flag"
	"fmt"
	"golang.org/x/crypto/ssh"
	"io/ioutil"
	"os"
	"strings"
	"sync"
)

type Result struct {
	Host    string //The string used to connect to the host
	Orig    string //The original host provided, may not contain port
	Message string //The output message received
	Status  bool   //Whether we were successful or failed
}

var inChan chan string
var outChan chan Result
var doneChan chan bool
var doneWait sync.WaitGroup

func main() {
	var authType = "pass"
	var authInfo []byte
	var authUser string

	optTargets := flag.String("targets", "", "File of targets to connect to (host:port).  Port is optional.")
	optOut := flag.String("out", "", "File to write our detailed results to.")
	optUser := flag.String("user", "", "Username to attempt.")
	optKey := flag.String("key", "", "File to use for key based auth.")
	optPass := flag.String("pass", "", "Password to use for auth.")
	optThreads := flag.Int("threads", 10, "Number of concurrent connections to attempt.")
	flag.Parse()

	if *optTargets == "" {
		fmt.Fprint(os.Stderr, "ERROR: Target file was not defined.\n")
		flag.PrintDefaults()
		return
	}

	if *optUser == "" {
		fmt.Fprint(os.Stderr, "ERROR: Username was not defined.\n")
		flag.PrintDefaults()
		return
	}
	authUser = *optUser

	if *optKey == "" && *optPass == "" {
		fmt.Fprint(os.Stderr, "ERROR: An authentication method (key or pass) was not defined.\n")
		flag.PrintDefaults()
		return
	}

	if *optKey != "" {
		authType = "key"
		tmpData, err := ioutil.ReadFile(*optKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: Unable to read authentication key file: %s\n", err.Error())
			return
		}
		authInfo = tmpData
	} else {
		authInfo = []byte(*optPass)
	}

	if *optOut == "" {
		fmt.Fprint(os.Stderr, "ERROR: Output file was not defined.\n")
	}
	
	outFile, err := os.Create(*optOut)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Unable to create output file: %s\n", err.Error())
	}
	defer outFile.Close()

	inChan = make(chan string, 10)
	outChan = make(chan Result)
	doneChan = make(chan bool)

	go runOutput(outFile)

	for i := 0; i < *optThreads; i++ {
		doneWait.Add(1)
		go runConnect(authType, authInfo, authUser)
	}

	err = runInput(*optTargets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Unable to open input file: %s\n", err.Error())
		return
	}

	signalDone(*optThreads)
	doneWait.Wait()
}

func signalDone(routines int) {
	for i := 0; i < routines; i++ {
		doneChan <- true
	}
}

func runInput(file string) error {
	inFile, err := os.Open(file)
	if err != nil {
		return err
	}
	defer inFile.Close()

	scanner := bufio.NewScanner(inFile)
	for scanner.Scan() {
		line := scanner.Text()
		inChan <- line
	}

	return nil
}

func runOutput(outFile *os.File) {
	linecount := 0

	for {
		result := <-outChan
		if result.Status {
			fmt.Printf("\033[32;1m%-20s\033[0m\t", result.Orig)
		} else {
			fmt.Printf("\033[31m%-20s\033[0m\t", result.Orig)
		}

		linecount++
		if linecount > 3 {
			fmt.Printf("\n")
			linecount = 0
		}

		outFile.WriteString(fmt.Sprintf("'%s','%t','%s'\n", result.Orig, result.Status, result.Message))
	}
}

func runConnect(authtype string, authdata []byte, user string) {
	for {
		select {
		case target := <-inChan:
			origtarget := target
			if !strings.Contains(target, ":") {
				target = target + ":22"
			}

			var err error
			switch authtype {
			case "pass":
				_, _, err = connectByPass(user, target, string(authdata))
			case "key":
				_, _, err = connectByCert(user, target, authdata)
			}

			message := "Successfully connected"
			status := true

			if err != nil {
				message = err.Error()
				status = false
			}

			outChan <- Result{
				Host:    target,
				Orig:    origtarget,
				Message: message,
				Status:  status,
			}

		case <-doneChan:
			doneWait.Done()
		}
	}
}

func connectByCert(user, host string, keyinfo []byte) (*ssh.Client, *ssh.Session, error) {
	signer, err := ssh.ParsePrivateKey(keyinfo)
	if err != nil {
		return nil, nil, err
	}

	conf := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
	}

	return connect(user, host, conf)
}

func connectByPass(user, host, pass string) (*ssh.Client, *ssh.Session, error) {
	conf := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{ssh.Password(pass)},
	}

	return connect(user, host, conf)
}

func connect(user, host string, conf *ssh.ClientConfig) (*ssh.Client, *ssh.Session, error) {
	conn, err := ssh.Dial("tcp", host, conf)
	if err != nil {
		return nil, nil, err
	}

	session, err := conn.NewSession()
	if err != nil {
		conn.Close()
		return nil, nil, err
	}

	return conn, session, nil
}
