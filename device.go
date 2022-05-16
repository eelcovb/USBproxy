// Methods for detecting USB printers
package main

import (
	"bufio"
	"encoding/gob"
	"fmt"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/eelcovb/goStar/star"
	"github.com/google/gousb"
)

// Printer defines a printer
// The printer type keeps information about a specific printer
// and its connections (both TCP as USB)
type Printer struct {
	Vendor       uint16
	Product      uint16
	Key          string // "unique" for device
	Manufacturer string
	Brand        string
	ProductName  string
	SerialNumber string
	Description  string
	Address      int
	Speed        string
	Bus          int
	Port         int
	TCPPort      int       // The TCP port for this device
	USBConnected bool      // True when the device is connected to USB
	TCPListening bool      // True when a TCP listener is running
	Active       bool      // True when spark is relaying data for this device
	Delete       bool      // True when the printer is marked for deletion
	ZeroConf     chan bool // A channel controlling the zeroConf service, set to true when ZC is enabled, false on closedown
	LastError    error     // The last error that was received for this printer

}

// Constants for timing and printer identification
const (
	discoveryDelayInMs = 500
	StarVendorID       = 0x0519
)

// VendorID Returns the VendorID in gousb.ID type
func (p *Printer) VendorID() gousb.ID {
	return gousb.ID(p.Vendor)
}

// ProductID Returns the VendorID in gousb.ID type
func (p *Printer) ProductID() gousb.ID {
	return gousb.ID(p.Product)
}

// PrinterList is a structure to keep track of all
// the printers, their query times etc
type PrinterList struct {
	Printers     map[string]*Printer
	PrinterTypes map[uint16]string // List of supported Vendor Id's with their brands
	StoragePath  string
}

// Cleanup removes a certain printer from the list
// Cleanup waits for a printer to come out of the server process
// It then cleans it from the map
func (pl *PrinterList) Cleanup(chP chan *Printer) {

	for {
		// Wait for a printer beeing returned on the channel
		pRemove := <-chP
		// Check if it actually needs to be removed
		if !pRemove.Delete {
			// This was passed along, push it back on the channel to be picked up by the server
			// FIXME: use 2 channels?
			chP <- pRemove
		} else {
			// pRemove is the printer we need to remove
			log.Printf("Received a signal on channel %v, printer %s needs to be removed",
				chP, pRemove.Description)
			delete(pl.Printers, pRemove.Key)
			if err := pl.Save(); err != nil {
				log.Printf("Failed to store state to %s: %s", pl.StoragePath, err)
			}
		}
	}
}

// Init initializes the printer list
func (pl *PrinterList) Init() {
	pl.Printers = make(map[string]*Printer)
}

// Save Encode via Gob to file
func (pl *PrinterList) Save() error {
	var err error
	if pl.StoragePath != "" {
		file, err := os.Create(pl.StoragePath)
		defer file.Close()
		if err == nil {
			encoder := gob.NewEncoder(file)
			err = encoder.Encode(pl)
			if err != nil {
				log.Printf("Error while encoding data: %s", err)
			}
		}
	} else {
		err = fmt.Errorf("Save(): storagePath not set")
	}
	return err
}

// Load Decode Gob file
func (pl *PrinterList) Load() error {
	if pl.StoragePath != "" {
		file, err := os.Open(pl.StoragePath)
		if err != nil {
			return err
		}
		decoder := gob.NewDecoder(file)
		err = decoder.Decode(pl)
		file.Close()
		return err
	}
	return fmt.Errorf("Load(): storagePath not set")
}

// USBKey generates an 'unique' USB key
func USBKey(Vendor, Product gousb.ID,
	Manufacturer, ProductName string,
	Bus, Address int) string {
	return fmt.Sprintf("%v%v%s%s%d",
		Vendor, Product, Manufacturer, ProductName, Bus)
}

func inList(name string, list []string) bool {
	for _, p := range list {
		if strings.Contains(name, p) {
			return true
		}
	}
	return false
}

// loadPrinterTypes loads the printer types to scan the USB bus for
func (pl *PrinterList) loadPrinterTypes(path string) error {

	if path == "" {
		return fmt.Errorf("Printer types file path was not set")
	}

	pl.PrinterTypes = make(map[uint16]string)

	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	lineCtr := 1
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		val := scanner.Text()
		if strings.HasPrefix(val, "#") || val == "" {
			continue
		} else {
			token := strings.Split(val, ":")
			if len(token) == 2 {
				if intVal, err := strconv.ParseUint(token[1], 16, 16); err != nil {
					return err
				} else {
					pl.PrinterTypes[uint16(intVal)] = token[0]
				}
			} else {
				return fmt.Errorf("%s:%d: Incorrect number of tokens, should be: <Brand>:<vendorID in Hex>",
					path, lineCtr)
			}

		}
		lineCtr++
	}
	return scanner.Err()
}

// IsSupported checks the array of supported printer types
// and returns true when the given vendor id is in the supported list
func (pl *PrinterList) IsSupported(vid uint16, pid uint16) bool {
	for _, val := range []uint16{vid, pid} {
		if _, hasKey := pl.PrinterTypes[val]; hasKey {
			return true
		}
	}
	return false
}

// SetSerial sets the serialNumber for the USB interface on a Star printer
//  to a random unqiue string
func (p *Printer) SetSerial() {

	newUSBID := star.MakeRandomID()
	log.Printf("New ID for %s will be: %s, sending instructions", p.Description, newUSBID)
	err := star.SendUsbIDToPrinter(
		newUSBID,
		p.Address,
		p.Bus,
		p.Port,
		true)

	if err != nil {
		log.Printf("Failed to set the serialNumber for %s: %s",
			p.Description, err)
	}
}

// Discover queries universal serial bus for devices
func (pl *PrinterList) Discover() {

	log.Printf("Running USB discovery process")
	// Initiate a context
	ctx := gousb.NewContext()
	defer ctx.Close()

	for {
		// Get the printers
		devs, _ := ctx.OpenDevices(func(desc *gousb.DeviceDesc) bool {
			// This function is called for every device present
			return pl.IsSupported(uint16(desc.Vendor), uint16(desc.Product))
		})

		// Register the devices to the list
		for _, d := range devs {

			manufacturer, _ := d.Manufacturer()
			productname, _ := d.Product()
			searchKey, _ := d.SerialNumber()

			// Check if it already exists or not
			if searchKey == "" {
				searchKey = USBKey(
					d.Desc.Vendor, d.Desc.Product, manufacturer, productname,
					d.Desc.Bus, d.Desc.Address)
			}

			if _, pInList := pl.Printers[searchKey]; !pInList {

				// New printer, add a defer for cleanup
				// defer d.Close()

				p := new(Printer)
				p.Vendor = uint16(d.Desc.Vendor)
				p.Product = uint16(d.Desc.Product)
				p.Manufacturer, _ = d.Manufacturer()
				p.Brand = pl.PrinterTypes[p.Vendor]
				p.ProductName, _ = d.Product()
				p.SerialNumber, _ = d.SerialNumber()
				p.Description = d.String()
				p.Bus = d.Desc.Bus
				p.Address = d.Desc.Address
				p.Speed = d.Desc.Speed.String()
				p.USBConnected = false
				p.TCPListening = false
				p.Delete = false
				p.Active = false

				if p.SerialNumber == "" {
					p.Key = USBKey(p.VendorID(), p.ProductID(), p.Manufacturer, p.ProductName, p.Bus, p.Address)
				} else {
					log.Printf("Device %s has a USB SerialNumber (%s) set, this will be used as unique identifier\n", p.Description, p.SerialNumber)
					p.Key = p.SerialNumber
				}

				// Add to target printer list
				pl.Printers[p.Key] = p

				log.Printf("Found printer %s (%s) (vid: %v pid: %v) at bus %d, address: %d, port: %d  speed: %s, sn: %s, desc: %s",
					p.ProductName, p.Manufacturer, p.Vendor, p.Product, p.Bus, p.Address, p.Port, p.Speed, p.SerialNumber, p.Description)

				// Save the settings to the storageFile
				pl.Save()
			}
			// Close the device
			d.Close()
		}
		// Delay a bit to be non CPU hogging and alow the scheduler to catch up
		runtime.Gosched()
		time.Sleep(time.Millisecond * discoveryDelayInMs)
	}
}
