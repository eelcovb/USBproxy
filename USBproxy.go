package main

import (
	"flag"
	"log"
	"runtime"
	"time"

	"github.com/eelcovb/goStar/star"
)

const (
	application          = "USBproxy"
	copyright            = "(c) 2022 Impero, LLC."
	version              = "0.4"
	storageFile          = "usbproxy.dat"
	printerTypesListFile = "printerTypes.txt"
	monitorMemory        = false
	mainLoopDelayInMs    = 500
	maxRoutines          = 5
	tcpStartPort         = 9100
	numberOfTCPPorts     = 10
	memoryCheckInterval  = 60
	waitForSerialAddSecs = 120
	bytesToMbytes        = 1048576.0
)

var debug bool

// Set to anything else than 0 when a secondary port needs to be active
var sPort int

// MemoryReporter Separate thread which reports on memory every
// MemoryCheckInterval seconds
func MemoryReporter() {

	if monitorMemory {
		var mem runtime.MemStats
		lastGC := time.Time{}

		for {
			// Load mem stats from runtime environment
			runtime.ReadMemStats(&mem)

			lastGC = time.Unix(0, int64(mem.LastGC))

			log.Printf("Stack  : %dMB From system: %dMB",
				mem.Alloc/bytesToMbytes, mem.Sys/bytesToMbytes)
			log.Printf("Heap   : %dMB From system: %dMB",
				mem.HeapAlloc/bytesToMbytes, mem.HeapSys/bytesToMbytes)
			log.Printf("Objects: %v allocated, freed: %v in use: %v",
				mem.Mallocs, mem.Frees, mem.Mallocs-mem.Frees)

			if lastGC.IsZero() {
				log.Printf("Garbage collection has not run yet")
			} else {
				log.Printf("Last garbage collection ran at %s", lastGC)
			}
			time.Sleep(time.Second * memoryCheckInterval)
		}
	}
}

func main() {
	// Push the memory checker on a separate check
	go MemoryReporter()

	log.Printf("*** %s %s %s\n", application, version, copyright)

	proxyMode := flag.String("proxy", "", "Format <myport>:<targethost>:<targetport>, runs a proxy server")
	dualPort := flag.Int("dualPort", 0, "If set, this secondary port is opened and its data inserted to the USB stream")
	debugMode := flag.Bool("debug", false, "Runs USBproxy in debug mode")
	zeroConfName := flag.String("zeroconf", "Star TSP654", "Define a zeroconf (bonjour) name for when spark runs as proxy")
	noStar := flag.Bool("nostar", false, "Do not detect star devices and set their USB-ID's if they don't have them set")
	flag.Parse()

	// Set global debug variable
	debug = *debugMode

	// Check if proxy mode is applicable
	if *proxyMode != "" {
		RunProxyMode(proxyMode, *zeroConfName)
		return
	}

	pl := new(PrinterList)
	// Init the list
	pl.Init()

	// Load the printer type
	if err := pl.loadPrinterTypes(printerTypesListFile); err != nil {
		log.Fatalf("Unable to load printer type definitions file: %s", err)
	} else {
		log.Printf("Loaded %d printer type definitions: %v", len(pl.PrinterTypes), pl.PrinterTypes)
	}

	sPort = *dualPort

	// Set the storagePath
	pl.StoragePath = storageFile

	if err := pl.Load(); err != nil {
		// No or incorrect printer data file found
		log.Printf("No or incorrect data file (%s) found: %s", storageFile, err)
		// Inverted logic, makes flag more reasonable :)
		if !*noStar {
			log.Printf("No previous printers set, scanning for star printers")
			starPrinterList := new(star.PrinterList)
			printersFound, err := starPrinterList.Discover()
			if err != nil {
				log.Printf("Failed to scan for Star printers for serial programming:%s", err)
			} else {
				if printersFound > 0 {
					log.Printf("%d Star printers found, checking if they need programming", printersFound)
					if n, err := starPrinterList.SetGenericIDForAll(); err != nil {
						log.Printf("Failed to program the serial numbers: %s", err)
					} else {
						log.Printf("%d Star printers programmed with a random serial#", n)
					}
				} else {
					log.Printf("No Star printers found")
				}
			}
		}
	} else {
		for _, p := range pl.Printers {
			log.Printf("Trying to reactivate printer %s (%s)",
				p.ProductName, p.Key)
			p.Active = false
		}
	}
	log.Printf("USB Printer watchdog activated")

	// Create a communication channel for printers
	chP := make(chan *Printer)

	// Start the discovery in the background
	go pl.Discover()

	// Start the cleanup in the background
	go pl.Cleanup(chP)

	for {
		for _, p := range pl.Printers {
			// Run through the printers and initiate servers if needed
			if !p.Active && !p.Delete {
				// Flag printer as active
				p.Active = true
				chP <- p
				go Server(chP)
			}
		}
		time.Sleep(time.Millisecond * mainLoopDelayInMs)
	}
}
