//go:build windows

package serialport

import (
	"errors"
	"strings"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

// windowsCandidates lists the COM ports the system knows
// (HKLM\HARDWARE\DEVICEMAP\SERIALCOMM) and gives those served by the FTDI
// driver their USB ids: the driver enumerates each adapter under
// HKLM\SYSTEM\CurrentControlSet\Enum\FTDIBUS as VID_0403+PID_6001+<serial>A
// with the port name under 0000\Device Parameters.
func windowsCandidates() ([]Candidate, error) {
	byPort := map[string]*Candidate{}
	var order []string
	comm, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DEVICEMAP\SERIALCOMM`, registry.READ)
	switch {
	case errors.Is(err, syscall.ERROR_FILE_NOT_FOUND):
		return nil, nil
	case err != nil:
		return nil, err
	}
	names, err := comm.ReadValueNames(0)
	if err != nil {
		_ = comm.Close()
		return nil, err
	}
	for _, n := range names {
		v, _, err := comm.GetStringValue(n)
		if err != nil || v == "" {
			continue
		}
		if _, ok := byPort[v]; !ok {
			byPort[v] = &Candidate{Path: v}
			order = append(order, v)
		}
	}
	_ = comm.Close()

	ftdi, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Enum\FTDIBUS`, registry.READ)
	if err == nil {
		devices, _ := ftdi.ReadSubKeyNames(0)
		_ = ftdi.Close()
		for _, dev := range devices {
			vendor, product, serial := parseFTDIBusName(dev)
			if vendor == 0 {
				continue
			}
			params, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Enum\FTDIBUS\`+dev+`\0000\Device Parameters`, registry.READ)
			if err != nil {
				continue
			}
			port, _, err := params.GetStringValue("PortName")
			_ = params.Close()
			if err != nil || port == "" {
				continue
			}
			c, ok := byPort[port]
			if !ok {
				c = &Candidate{Path: port}
				byPort[port] = c
				order = append(order, port)
			}
			c.VendorID, c.ProductID, c.Serial = vendor, product, serial
		}
	}
	cands := make([]Candidate, 0, len(order))
	for _, p := range order {
		cands = append(cands, *byPort[p])
	}
	return cands, nil
}

// parseFTDIBusName reads "VID_0403+PID_6001+AB0JQ5W9A".
func parseFTDIBusName(name string) (vendor, product uint16, serial string) {
	parts := strings.Split(name, "+")
	if len(parts) < 2 {
		return 0, 0, ""
	}
	v, ok := parseUSBID(strings.TrimPrefix(strings.ToUpper(parts[0]), "VID_"))
	if !ok {
		return 0, 0, ""
	}
	p, _ := parseUSBID(strings.TrimPrefix(strings.ToUpper(parts[1]), "PID_"))
	if len(parts) > 2 {
		serial = strings.TrimSuffix(parts[2], "A")
	}
	return v, p, serial
}
