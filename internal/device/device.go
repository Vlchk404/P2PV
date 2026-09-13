// Package device wraps the virtual network interface.
//
// This is the piece the earlier P2PV prototypes were missing. They assigned
// virtual addresses to a Microsoft KM-TEST loopback adapter, which cannot carry
// a packet off the machine by definition -- the address appeared in ipconfig,
// peers pinged, and nothing ever arrived. A real tunnel interface is what makes
// the virtual LAN actually exist.
//
// The backend is wireguard-go's tun package: Wintun on Windows (a signed,
// MIT-licensed driver that installs from a DLL, with no separate installer) and
// /dev/net/tun on Linux. Both are layer 3, so what comes out of Read is a bare
// IP packet with no Ethernet header.
//
// The Device interface exists so a layer 2 TAP backend can be added later
// without touching the data plane: ARP, non-IP protocols and true Ethernet
// broadcast would come with it, at the cost of a driver installer.
package device

import (
	"fmt"

	"golang.zx2c4.com/wireguard/tun"
)

// Device is a virtual network interface carrying IP packets.
type Device interface {
	// Read fills buf with one inbound IP packet from the OS.
	Read(buf []byte) (int, error)
	// Write sends one IP packet to the OS.
	Write(packet []byte) (int, error)
	// Name returns the interface name.
	Name() string
	// Close tears the interface down.
	Close() error
}

// tunDevice adapts wireguard-go's tun.Device to Device.
//
// That API is batched and reserves a per-packet offset for its own headers, so
// the adapter hides the batching and keeps one scratch buffer per direction.
// Read and Write are each called from a single dedicated goroutine, so the
// scratch buffers need no locking.
type tunDevice struct {
	dev  tun.Device
	name string

	readBufs  [][]byte
	readSizes []int
	writeBuf  []byte
}

const (
	// offset is the headroom wireguard-go's API expects ahead of each packet.
	offset = 16
	// maxPacket is the largest frame we handle, MTU plus headroom.
	maxPacket = 2048
)

func wrap(dev tun.Device) (Device, error) {
	name, err := dev.Name()
	if err != nil {
		dev.Close()
		return nil, fmt.Errorf("device: read interface name: %w", err)
	}
	return &tunDevice{
		dev:       dev,
		name:      name,
		readBufs:  [][]byte{make([]byte, maxPacket)},
		readSizes: make([]int, 1),
		writeBuf:  make([]byte, maxPacket),
	}, nil
}

func (d *tunDevice) Read(buf []byte) (int, error) {
	for {
		n, err := d.dev.Read(d.readBufs, d.readSizes, offset)
		if err != nil {
			return 0, err
		}
		if n == 0 {
			// A wake-up with no packet; keep waiting rather than reporting EOF.
			continue
		}
		size := d.readSizes[0]
		if size > len(buf) {
			// Larger than our buffer: drop it rather than truncate, which would
			// hand a corrupt packet to the peer.
			continue
		}
		copy(buf, d.readBufs[0][offset:offset+size])
		return size, nil
	}
}

func (d *tunDevice) Write(packet []byte) (int, error) {
	if len(packet)+offset > len(d.writeBuf) {
		return 0, fmt.Errorf("device: packet of %d bytes exceeds buffer", len(packet))
	}
	copy(d.writeBuf[offset:], packet)
	n, err := d.dev.Write([][]byte{d.writeBuf[:offset+len(packet)]}, offset)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	return len(packet), nil
}

func (d *tunDevice) Name() string { return d.name }

func (d *tunDevice) Close() error { return d.dev.Close() }
