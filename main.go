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

// A struct to hold our results before we output them
type Result struct {
	Host    string //The string used to connect to the host
	Message string //The output message received
	Output  string //The output of the command run, if any
	Status  bool   //Whether we were successful or failed
}

// A channel to hold our input data.  It will be one target string per line
var inChan chan string

// A channel of Result structs to hold our output before written to the file / stdout
var outChan chan Result

// A channel we'll use to signal when we're out of input so our goroutines can stop
var runDoneChan chan bool
var outDoneChan chan bool

// We'll use this waitgroup to track the total number of routines we have started
// so that everything can stop gracefully
var runDoneWait sync.WaitGroup
var outDoneWait sync.WaitGroup

func main() {
	// Some variables to hold our authentication types.
	var authType = "pass"
	var authInfo []byte
	var authUser string

	// Let's setup our flags and parse them
	optTargets := flag.String("targets", "", "File of targets to connect to (host:port).  Port is optional.")
	optOut := flag.String("out", "", "File to write our detailed results to.")
	optUser := flag.String("user", "", "Username to attempt.")
	optKey := flag.String("key", "", "PEM encoded key file to use for auth. (Note: Key or Pass may be used, not both)")
	optPass := flag.String("pass", "", "Password to use for auth.")
	// Using the word threads here so it makes sense to end users, but we're really using goroutines
	optThreads := flag.Int("threads", 10, "Number of concurrent connections to attempt.")
	optCmd := flag.String("cmd", "", "Command to run on remote systems. Newlines will be replaced with <br>.")
	flag.Parse()

	// If we didn't get any targets, print an error.
	if *optTargets == "" {
		fmt.Fprint(os.Stderr, "ERROR: Target file was not defined.\n")
		flag.PrintDefaults()
		return
	}

	// A username is also mandatory for connection
	if *optUser == "" {
		fmt.Fprint(os.Stderr, "ERROR: Username was not defined.\n")
		flag.PrintDefaults()
		return
	}
	authUser = *optUser

	// We need either a key or a password to connect, otherwise error
	if *optKey == "" && *optPass == "" {
		fmt.Fprint(os.Stderr, "ERROR: An authentication method (key or pass) was not defined.\n")
		flag.PrintDefaults()
		return
	}

	// If we got a key, let's change the type to "key" and read in the key file.
	if *optKey != "" {
		authType = "key"
		tmpData, err := ioutil.ReadFile(*optKey)

		// If we couldn't open the key file, error out.
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: Unable to read authentication key file: %s\n", err.Error())
			return
		}

		// Otherwise, take the key data and put it in the proper variable
		authInfo = tmpData
	} else {
		// If we got a password, convert it to a byte slice and put it in the variable
		authInfo = []byte(*optPass)
	}

	// If we didn't get an output file, error out.
	if *optOut == "" {
		fmt.Fprint(os.Stderr, "ERROR: Output file was not defined.\n")
	}

	// If we can't create the output file, error out
	outFile, err := os.Create(*optOut)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Unable to create output file: %s\n", err.Error())
	}
	defer outFile.Close()

	// Setup our input channels, with out and done being async, but limit in to
	// the number of goroutines we're going to use
	inChan = make(chan string, *optThreads)
	outChan = make(chan Result)
	runDoneChan = make(chan bool, *optThreads)
	outDoneChan = make(chan bool, 1)

	// Startup a goroutine that will handle our output (stdout and file)
	go runOutput(outFile)

	// Startup goroutines for the number the user gave us.  Each will connect to hosts
	// and try and run a command if one was provided.
	for i := 0; i < *optThreads; i++ {
		go runConnect(authType, authInfo, authUser, *optCmd, i)
	}

	// This function loops through all of our input and adds it to the proper
	// channel.  If we can't open the intput file, we should error and die.
	err = runInput(*optTargets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Unable to open input file: %s\n", err.Error())
		return
	}

	// Finally, let's signal all of our goroutines (+1 is for output routine) and
	// tell them we're done, then wait for them all to shutdown
	signalDone(*optThreads)
	runDoneWait.Wait()

	// Now let's signal the output thread we're done and to shutdown when it's done
	outDoneChan <- true
	outDoneWait.Wait()
}

// This function signals that we're done by sending data down the runDoneChannel that
// we're using as a signal.  It will send that as many times as we have routines
// because we need to make sure each routine gets it at least once.
func signalDone(routines int) {
	for i := 0; i < routines; i++ {
		runDoneChan <- true
	}
}

// This function loops through an input file and adds each line to a channel
// that will be consumed by the runConnect functions.
func runInput(file string) error {
	// Open the file and if there's an error, return it
	inFile, err := os.Open(file)
	if err != nil {
		return err
	}
	defer inFile.Close()

	// Setup a scanner to read the file line by line
	scanner := bufio.NewScanner(inFile)
	for scanner.Scan() {
		// Get the line from the scanner
		line := scanner.Text()

		// Add it to our channel
		inChan <- line
	}

	// When we're done with the file, return that we're complete.
	return nil
}

// A function (probably a single goroutine) that handles writing our results to
// both the screen and an output file.  Takes an argument of a file connection
// And then starts a permanent loop that waits for data on the outChan channel
// of result objects.
func runOutput(outFile *os.File) {
	// We're going to increase the waitgroup number so the main routine knows when
	// everything is done.
	outDoneWait.Add(1)

	// Write the header row to our CSV
	outFile.WriteString("'Host','Success','Message','Output'\n")

	// Create a counter, because we'll want to put in line returns sometimes
	linecount := 0
	for {
		select {
		// Get some data from our output channel
		case result := <-outChan:
			// We're going to print the IP / target, if we were successful we'll print
			// it in green, if not we'll print it in red.  Full details will be in the
			// output file, but it's nice to provide quick feedback.
			if result.Status {
				fmt.Printf("\033[32;1m%-20s\033[0m\t", result.Host)
			} else {
				fmt.Printf("\033[31m%-20s\033[0m\t", result.Host)
			}
			// Increase the line counter, if it's greater than 3 then we'll just
			// print a newline and reset the counter.  This is so we have limited
			// numbers on each line and try to line up the columns.
			linecount++
			if linecount > 3 {
				fmt.Printf("\n")
				linecount = 0
			}

			// Finally, let's write the string to our output file.
			outFile.WriteString(fmt.Sprintf("'%s','%t','%s','%s'\n", result.Host, result.Status, result.Message, result.Output))

		// We'll use runDoneChan to signal that the program is complete (probably out of input).
		// Once we're done printing all of our output, let's signal that we're done.
		case <-outDoneChan:
			outDoneWait.Done()
			return
		}
	}
}

// Starts a loop that listens for targets on the inChan channel.  When a target
// is in the channel, it pops it off, and connects to the target using
// authentication information passed in as arguments.  Authtype should be either
// "pass" or "key" to signal how we should connect.  It will also run a command
// if one is provided and gather the output.  There are no returns, but when
// complete passes a Result struct down the outChan channel.
//
// To end this loop, any data should be sent down the runDoneChan to signal program
// complete.
func runConnect(authtype string, authdata []byte, user, cmd string, num int) {
	// Let's increase the WaitGroup we have so main knows how many goroutines are
	// running.
	runDoneWait.Add(1)

	for {
		select {
		//In the event we have a target, let's process it.
		case target := <-inChan:
			// Because we may add a port, let's use a temp string so we can return what the user gave us
			fulltarget := target
			// Add port 22 to the target if we didn't get a port from the user.
			if !strings.Contains(fulltarget, ":") {
				fulltarget = fulltarget + ":22"
			}

			// Declare some variables to hold our SSH connection and any errors
			var err error
			var session *ssh.Session

			// Depending on the authentication type, run the correct connection function
			switch authtype {
			case "pass":
				_, session, err = connectByPass(user, fulltarget, string(authdata))
			case "key":
				_, session, err = connectByCert(user, fulltarget, authdata)
			}

			// Let's assume that we connected successfully and declare the data as such
			result := Result{
				Host:    target,
				Message: "Successfully connected",
				Status:  true,
				Output:  "",
			}

			// If we got an error, let's set the data properly
			if err != nil {
				result.Message = err.Error()
				result.Status = false
			}

			// If we didn't get an error and we have a command to run, let's do it.
			if err == nil && cmd != "" {
				// Execute the command
				result.Output, err = executeCommand(cmd, session)
				if err != nil {
					// If we got an error, let's give the user some output.
					result.Output = "Script Error: " + err.Error()
				}
			}

			// Finally, let's pass our result to the proper channel to write out to the user
			outChan <- result

		// We'll use doneChan to signal that the program is complete (probably out of input).
		// When we get data on this channel as a signal, we'll signal that this routine is done
		// so main knows when they're all complete.  Finally, we'll return
		case <-runDoneChan:
			runDoneWait.Done()
			return
		default:
		}
	}
}

// Executes a command on an SSH session struct, return an error if there is one
func executeCommand(cmd string, session *ssh.Session) (string, error) {
	//Runs CombinedOutput, which takes cmd and returns stderr and stdout of the command
	out, err := session.CombinedOutput(cmd)
	if err != nil {
		return "", err
	}

	// Convert our output to a string
	tmpOut := string(out)
	tmpOut = strings.Replace(tmpOut, "\n", "<br>", -1)

	// Return a string version of our result
	return tmpOut, nil
}

// Connects to a target via SSH using a certificate
func connectByCert(user, host string, keyinfo []byte) (*ssh.Client, *ssh.Session, error) {
	//Parse the private key, return if there is an error
	signer, err := ssh.ParsePrivateKey(keyinfo)
	if err != nil {
		return nil, nil, err
	}

	//Build the configuration struct we need
	conf := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
	}

	// Connect and return the result
	return connect(user, host, conf)
}

// Connects to a target using SSH with a password in a string.
func connectByPass(user, host, pass string) (*ssh.Client, *ssh.Session, error) {
	//Build our config with the password
	conf := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{ssh.Password(pass)},
	}

	// Connect and return the result
	return connect(user, host, conf)
}

// Connects to a host using a SSH configuration struct.  Returns the
// SSH client and session structs and an error if there was one.
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
