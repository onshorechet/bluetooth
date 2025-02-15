//go:build !baremetal

// Some documentation for the BlueZ D-Bus interface:
// https://git.kernel.org/pub/scm/bluetooth/bluez.git/tree/doc

package bluetooth

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/godbus/dbus/v5"
)

const defaultAdapter = "hci0"

type Adapter struct {
	id                   string
	scanCancelChan       chan struct{}
	bus                  *dbus.Conn
	bluez                dbus.BusObject // object at /
	adapter              dbus.BusObject // object at /org/bluez/hciX
	address              string
	defaultAdvertisement *Advertisement

	devicesMu sync.Mutex // protects the devices map
	devices   map[dbus.ObjectPath]*Device

	connectHandler func(device Device, connected bool)
}

// NewAdapter creates a new Adapter with the given ID.
//
// Make sure to call Enable() before using it to initialize the adapter.
func NewAdapter(id string) *Adapter {
	return &Adapter{
		id:             id,
		connectHandler: func(device Device, connected bool) {},
		devices:        make(map[dbus.ObjectPath]*Device),
	}
}

// DefaultAdapter is the default adapter on the system. On Linux, it is the
// first adapter available.
//
// Make sure to call Enable() before using it to initialize the adapter.
var DefaultAdapter = NewAdapter(defaultAdapter)

// Enable configures the BLE stack. It must be called before any
// Bluetooth-related calls (unless otherwise indicated).
func (a *Adapter) EnableWithContext(ctx context.Context) (err error) {
	bus, err := dbus.SystemBus()
	if err != nil {
		return err
	}
	a.bus = bus
	a.bluez = a.bus.Object("org.bluez", dbus.ObjectPath("/"))
	a.adapter = a.bus.Object("org.bluez", dbus.ObjectPath("/org/bluez/"+a.id))
	addr, err := a.adapter.GetProperty("org.bluez.Adapter1.Address")
	if err != nil {
		if err, ok := err.(dbus.Error); ok && err.Name == "org.freedesktop.DBus.Error.UnknownObject" {
			return fmt.Errorf("bluetooth: adapter %s does not exist", a.adapter.Path())
		}
		return fmt.Errorf("could not activate BlueZ adapter: %w", err)
	}
	addr.Store(&a.address)
	return a.enablePropsChangedWatch(ctx)
}

func (a *Adapter) Enable() (err error) {
	return a.EnableWithContext(context.Background())
}

func (a *Adapter) enablePropsChangedWatch(ctx context.Context) (err error) {
	// Already start watching for property changes. We do this before reading
	// the Connected property below to avoid a race condition: if the device
	// were connected between the two calls the signal wouldn't be picked up.
	signal := make(chan *dbus.Signal)
	a.bus.Signal(signal)
	propertiesChangedMatchOptions := []dbus.MatchOption{dbus.WithMatchInterface("org.freedesktop.DBus.Properties")}
	a.bus.AddMatchSignal(propertiesChangedMatchOptions...)

	// Wait until the device has connected.
	go func() {
		defer close(signal)
		defer a.bus.RemoveMatchSignal(propertiesChangedMatchOptions...)
		defer a.bus.RemoveSignal(signal)
		for {
			select {
			case sig := <-signal:
				switch sig.Name {
				case "org.freedesktop.DBus.Properties.PropertiesChanged":
					a.handlePropertiesChanged(sig)
				}
			case <-ctx.Done():
				err = errors.New("bluetooth: failed to connect: context canceled")
				return
			}
		}
	}()

	return nil
}

func (a *Adapter) handlePropertiesChanged(sig *dbus.Signal) {
	interfaceName := sig.Body[0].(string)
	if interfaceName != "org.bluez.Device1" {
		return
	}

	a.devicesMu.Lock()
	defer a.devicesMu.Unlock()
	if _, ok := a.devices[sig.Path]; !ok {
		return
	}

	changes := sig.Body[1].(map[string]dbus.Variant)
	connected, ok := changes["Connected"].Value().(bool)
	if !ok {
		return
	}

	if !connected {
		a.devices[sig.Path].link.disconnect()
	}

	a.devices[sig.Path].link.connect()
}

func (a *Adapter) Address() (MACAddress, error) {
	if a.address == "" {
		return MACAddress{}, errors.New("adapter not enabled")
	}
	mac, err := ParseMAC(a.address)
	if err != nil {
		return MACAddress{}, err
	}
	return MACAddress{MAC: mac}, nil
}
