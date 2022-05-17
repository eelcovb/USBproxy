package main

import (
	"fmt"
	"log"
	"net"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/google/gousb"
	"github.com/grandcat/zeroconf"
)

// General settings
const (
	DumpMode       = false
	SleepInMs      = 100
	DebugProxy     = true
	NoReadDelay    = 0
	ShortReadDelay = 100
	LongReadDelay  = 200
	SServerTimeout = 100  // ms
	pWait, pResume = 8, 9 // communication flags for the ProxyLock Channel
)

// ServerError types
type ServerError int

// FIXME: unsafe method - use a channel
var SSup bool = false

// The ProxyLock channel keeps track on side channel communications
// The secondaryServer will set pWait when it's using the USB channel
// and pResume when things are allowed to be resumed
var ProxyLock = make(chan int, 1)

// Error types
const (
	ErrorUSB ServerError = iota
	ErrorTCP
)

const (
	failedConnectionLimit = 15
	connectionRetryDelay  = time.Second * 30
	buffSize              = 1024
)

// SBuffer
// Sized buffer, keeps track on the intended  size of what's in the buffer
type SBuffer struct {
	Buffer []byte
	Len    int
}

func NewBuffer(size int) *SBuffer {
	buf := make([]byte, size)
	buffer := &SBuffer{
		Buffer: buf,
		Len:    0, // it's empty because it's new
	}
	return buffer
}

func getInOutEndpoints(intf *gousb.Interface) (in, out int) {

	endpoints := intf.Setting.Endpoints
	for _, endpoint := range endpoints {
		if endpoint.Direction == gousb.EndpointDirectionIn {
			in = endpoint.Number
		} else {
			out = endpoint.Number
		}
	}

	return in, out
}

func dumpStream(s string) {
	if DumpMode {
		log.Println(s)
	}
}

func flush(buf *[]byte) {
	b := *buf
	for i := 0; i < cap(b); i++ {
		b[i] = '\x00'
	}
}

// ProxyIO takes a data In function and a data Out function
// It monitors the data in, and sends the data out using the given out function
// The connError channel will be set when there's an error in either stream
// The descIn and descOut are used for logging information
// If debug is set, all data packages are shown
// The delay on read specifies the number of milliseconds to delay the read loop
// This is useful for noisy equipment hogging the CPU
func ProxyIO(in, out func([]byte) (int, error),
	descIn, descOut string,
	debug bool,
	delayOnRead int,
	bufferSize int,
	connError chan error) {

	var err error
	buf := make([]byte, bufferSize)
	var bytesRead, bytesWrite int

	log.Printf("ProxyIO() for %s ---> %s using channel %v with read delay %dms",
		descIn, descOut, connError, delayOnRead)

	for err == nil {
		if descIn == "usb" {
			// traffic on secondary channel, hold
			lockStatus := <-ProxyLock
			if lockStatus == pWait {
				log.Printf("ProxyIO() secondaryServer initiated pWait for USB-in\n")
				// wait until we get the next signal
				lockStatus = <-ProxyLock
				log.Printf("ProxyIO() pWait lock released\n")
			}
		}
		bytesRead, err = in(buf[:])
		if err != nil {
			continue
		}
		if bytesRead > 0 {
			if descOut == "usb" {
				// traffic on secondary channel, hold
				lockStatus := <-ProxyLock
				if lockStatus == pWait {
					log.Printf("ProxyIO() secondaryServer initiated pWait for USB-out\n")
					// wait until we get the next signal
					lockStatus = <-ProxyLock
					log.Printf("ProxyIO() pWait lock released\n")
				}
			}
			bytesWrite, err = out(buf[:bytesRead])
			if debug {
				log.Printf("%s ---- (%d/%d) bytes ----> %s [%v]",
					descIn, bytesRead, bytesWrite, descOut, buf[:bytesRead])
			}
		}
		runtime.Gosched()
		// Delay if a delay is set
		if delayOnRead > 0 {
			time.Sleep(time.Millisecond * time.Duration(delayOnRead))
		}
	}
	// Send an error on the channel
	connError <- err
}

// parseProxySettings parses the proxy commandline settings
// returns the listenIp, targetHost, targetPort or error and empty strings
func parseProxySettings(proxySettings *string) (*string, *string, *string, error) {

	settings := strings.Split(*proxySettings, ":")
	if len(settings) != 3 {
		return nil, nil, nil, fmt.Errorf("Proxy settings incorrect, \"%s\" incorrectly formatted", *proxySettings)
	}
	return &settings[0], &settings[1], &settings[2], nil
}

// RunProxyMode sets the server into proxy mode, all traffic is forwarded
func RunProxyMode(proxySettings *string, zeroConfName string) {

	listenPort, targetHost, targetPort, err := parseProxySettings(proxySettings)
	if err != nil {
		log.Fatalf("Could not start proxy mode: %s", err)
	}

	log.Printf("Enabling proxy mode: proxying port %s to %s:%s",
		*listenPort, *targetHost, *targetPort)

	targetConn, err := net.Dial("tcp", fmt.Sprintf("%s:%s", *targetHost, *targetPort))
	if err != nil {
		log.Printf("Could not reach %s:%s :%s", *targetHost, *targetPort, err)
		return
	}

	log.Printf("Succesfully connected to %s:%s", *targetHost, *targetPort)

	defer targetConn.Close()

	// Setup listener
	tcpListener, err := net.Listen("tcp", fmt.Sprintf(":%s", *listenPort))
	if err != nil {
		log.Fatalf("Could not start listener at port %s: %s", *listenPort, err)
	}
	defer tcpListener.Close()
	log.Printf("Listener listening on port %s", *listenPort)

	// Announcement on zeroconf
	log.Printf("Announcing service on zeroconf as: %s (emulating Star printer) on port %s", zeroConfName, *targetPort)
	portNr, err := strconv.Atoi(*listenPort)
	if err != nil {
		log.Printf("Unable to start zeroconf, %s could not be converted to correct port type: %s", *listenPort, err)
	} else {
		server, err := zeroconf.Register(zeroConfName, "_pdl-datastream._tcp", "local.", portNr,
			[]string{"txtv=0", "note=spark proxy", "product=" + zeroConfName, "ty=" + zeroConfName}, nil)
		if err != nil {
			log.Printf("Unable to start zeroconf service for %s :%s\n", zeroConfName, err)
		} else {
			log.Printf("Zeroconf active")
		}
		defer server.Shutdown()
	}

	// Wait for incoming connection
	localConn, err := tcpListener.Accept()
	defer localConn.Close()

	if err != nil {
		log.Fatalf("Failed to connect to incoming connection: %s", err)
		return
	}
	log.Printf("Incoming connection from: %s, enabling proxy process", localConn.RemoteAddr().String())

	// Create a blocking channel
	errChannel := make(chan error)

	// Handle target incoming
	go ProxyIO(targetConn.Read, localConn.Write,
		targetConn.RemoteAddr().String(),
		localConn.RemoteAddr().String(),
		debug,
		NoReadDelay,
		buffSize,
		errChannel)

	// Handle local incoming
	go ProxyIO(localConn.Read, targetConn.Write,
		localConn.RemoteAddr().String(),
		targetConn.RemoteAddr().String(),
		debug,
		NoReadDelay,
		buffSize,
		errChannel)

	// Block until  an error is set on the channel
	connError := <-errChannel

	log.Fatalf("Proxy finished due to a connection error:  %s", connError)
}

// ActivateBonjour Registers given printer as bonjour device
func ActivateBonjour(p *Printer) {

	instance := p.Brand + " " + p.ProductName

	server, err := zeroconf.Register(instance, "_pdl-datastream._tcp", "local.", p.TCPPort,
		[]string{"txtv=0", "note=" + p.SerialNumber, "product=" + instance,
			"ty=" + instance}, nil)
	if err != nil {
		log.Printf("Unable to start bonjour service for %s (%s):%s\n", p.SerialNumber, p.Description, err)
	}
	defer server.Shutdown()

	log.Printf("Printer %s (%s) is registered with zeroconf (port %d)\n", p.SerialNumber, p.Description, p.TCPPort)

	// Block on signal until false is received on the ZeroConf channel
	<-p.ZeroConf

	log.Printf("Zeroconf service for %s (%s) received termination signal", p.SerialNumber, p.Description)
}

// SecondaryServer
// Implements a secondary TCP server at given port
// Will run a separate proxy for its communication
func SecondaryServer(port int, usbIn, usbOut func([]byte) (int, error)) {

	log.Printf("SecondaryServer: Opening secondary port %d\n", port)
	sPortListener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Printf("SecondaryServer: unable to open port %d: %s\n", port, err)
		return
	}
	defer sPortListener.Close()

	SSup = true

	// Service incoming connections
	for SSup {

		// Accept incoming connection
		// This holds until something is going on
		conn, err := sPortListener.Accept()
		if err != nil {
			log.Printf("SecondaryServer: failed to connect to incoming connection: %s", err)
			return
		}
		log.Printf("SecondaryServer: Incoming connection from %s\n", conn.RemoteAddr())

		buf := NewBuffer(1024)
		// Read it out in one try
		buf.Len, err = conn.Read(buf.Buffer)
		log.Printf("SecondaryServer: received %d bytes from %s: [%s]\n",
			buf.Len,
			conn.RemoteAddr(),
			buf.Buffer)

		// Lock the proxies
		ProxyLock <- pWait

		// Send it to the USB port
		_, err = usbOut(buf.Buffer)

		if err == nil {

			// Delay a little bit
			time.Sleep(time.Millisecond * SServerTimeout)

			// Readout the USB port
			buf.Len, err = usbIn(buf.Buffer)

			if err == nil {
				// send back the response and close the channel
				conn.Write(buf.Buffer)
				fmt.Printf("SecondaryServer: sent %d bytes from USB feed to %s: [%s]\n",
					buf.Len, conn.RemoteAddr(), buf.Buffer)
			} else {
				log.Printf("SecondaryServer: failed to receive data from USB endpoint: %s\n", err)
			}
		} else {
			log.Printf("SecondaryServer: failed to send data to USB endpoint: %s\n", err)
		}

		// unlock the proxies
		ProxyLock <- pResume

		// Close the connection - this is intended behavior for POS printers
		conn.Close()
	}
	sPortListener.Close()
	log.Printf("SeconddaryServer: Shutdown\n")
}

// Server runs a tcp server
// and sends back and forth the data from the USB bus using the ProxyIO() function
// It uses a channel to report back when something happened
func Server(chP chan *Printer) {

	// Receive the printer to start a server for over the channel
	p := <-chP

	// Enable deferred adjustment of active flag
	deActivate := func() {
		p.Active = false
		p.Delete = true

		// Send a message to the ZeroConf channel that that service should be discontinued
		if p.ZeroConf != nil {
			p.ZeroConf <- false
		}
		log.Printf("Printer service %s deactivated, releasing channel %v", p.Key, chP)
		// Return this printer to the main thread
		chP <- p
	}

	defer deActivate()

	// This printer should not be deleted
	if p.Delete {
		deActivate()
	}

	// Setup USB link for communication
	log.Printf("Open device %v/%v bus %d port %d at %d (speed: %s)..\n",
		p.Product, p.Vendor, p.Bus, p.Port, p.Address, p.Speed)

	ctx := gousb.NewContext()
	defer ctx.Close()

	// Select the device
	devs, err := ctx.OpenDevices(func(desc *gousb.DeviceDesc) bool {
		return desc.Vendor == p.VendorID() &&
			desc.Product == p.ProductID()
	})

	if len(devs) == 0 {
		log.Printf("Device %s (%s) has gone away", p.ProductName, p.Key)
		return
	}

	if err != nil {
		log.Printf("OpenDevices(): %s", err)
		return
	}

	device := new(gousb.Device)

	for _, dev := range devs {
		defer dev.Close()
		if dev.String() == p.Description {
			device = dev
		}
	}

	if device == nil {
		log.Printf("OpenDevices(): %s", err)
		return
	}

	// Cleanup, the device needs to be closed when server is ready
	// Fixme: Double close?
	defer device.Close()

	// Claim the default interface
	intf, done, err := device.DefaultInterface()
	if err != nil {
		log.Printf("%s.DefaultInterface(): %v", device, err)
		return
	}
	defer intf.Close()
	defer done()

	// Get the endpoints
	in, out := getInOutEndpoints(intf)

	// Open the in endpoint
	epIn, err := intf.InEndpoint(in)
	if err != nil {
		log.Printf("%s.InEndpoint(%d): %v", intf, in, err)
		return
	}

	// Open the out endpoint
	epOut, err := intf.OutEndpoint(out)
	if err != nil {
		log.Printf("%s.OutEndpoint(%d): %s", intf, out, err)
		return
	}

	p.USBConnected = true

	shutdownSS := func() { SSup = false }

	// Check if we need to set up a side channel
	if sPort > 0 {
		// create a channel to work in separate communications
		// initiate the server
		go SecondaryServer(sPort, epIn.Read, epOut.Write)
		defer shutdownSS()
	}

	// Determine buffer size
	maxUsbInBufferSize := 10 * epIn.Desc.MaxPacketSize

	log.Printf("Scanning for available TCP port..")

	port := tcpStartPort
	tcpListener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))

	for err != nil {
		port++
		tcpListener, err = net.Listen("tcp", fmt.Sprintf(":%d", port))
	}

	defer tcpListener.Close()

	p.TCPPort = port
	p.TCPListening = true

	// Create a channel for the zeroconf service
	p.ZeroConf = make(chan bool)
	// Start the zero conf service
	go ActivateBonjour(p)

	log.Printf("Listening for incoming connection on port %d for device %s (%v/%v)",
		p.TCPPort, p.ProductName, p.Vendor, p.Product)

	// Accept incoming connection
	conn, err := tcpListener.Accept()
	if err != nil {
		log.Printf("Failed to connect to incoming connection: %s", err)
		return
	}

	defer conn.Close()

	log.Printf("Incoming connection from: %s, buffer size set to: %d bytes (USB endpoint limited)",
		conn.RemoteAddr().String(), maxUsbInBufferSize)

	// Create a channel to be noticed of any communication errors
	errChannel := make(chan error)

	readDelay := ShortReadDelay
	// Check if there is a star device connected
	if p.Vendor == StarVendorID {
		// Star device
		readDelay = LongReadDelay
		log.Printf("Star device detected, setting read delay to %dms", readDelay)
	}

	// Set up a ProxyIO from USB to TCP
	// Specifically set a read delay for busy USB devices
	go ProxyIO(epIn.Read, conn.Write, "USB", conn.RemoteAddr().String(),
		debug, readDelay, maxUsbInBufferSize, errChannel)

	// Set up a ProxyIO from TCP to USB
	go ProxyIO(conn.Read, epOut.Write, conn.RemoteAddr().String(), "USB",
		debug, NoReadDelay, maxUsbInBufferSize, errChannel)

	connErr := <-errChannel

	log.Printf("A connection error has occured: %s, device %s usb released", connErr, device.Desc)
}
