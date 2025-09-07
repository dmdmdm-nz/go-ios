package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver"
	"github.com/danielpaulus/go-ios/ios"
	log "github.com/sirupsen/logrus"
	"golang.org/x/exp/maps"
)

var netClient = &http.Client{
	Timeout: time.Millisecond * 200,
}

func CloseAgent() error {
	_, err := netClient.Get(fmt.Sprintf("http://%s:%d/shutdown", ios.HttpApiHost(), ios.HttpApiPort()))
	if err != nil {
		return fmt.Errorf("CloseAgent: failed to send shutdown request: %w", err)
	}
	return nil
}

func IsAgentRunning() bool {
	resp, err := netClient.Get(fmt.Sprintf("http://%s:%d/health", ios.HttpApiHost(), ios.HttpApiPort()))
	if err != nil {
		return false
	}
	return resp.StatusCode == http.StatusOK
}
func WaitUntilAgentReady() bool {
	for {
		slog.Info("Waiting for go-ios agent to be ready...")
		resp, err := netClient.Get(fmt.Sprintf("http://%s:%d/ready", ios.HttpApiHost(), ios.HttpApiPort()))
		if err != nil {
			return false
		}
		if resp.StatusCode == http.StatusOK {
			slog.Info("Go-iOS Agent is ready")
			return true
		}
	}
}

func RunAgent(mode string, args ...string) error {
	if IsAgentRunning() {
		return nil
	}
	slog.Info("Go-iOS Agent not running, starting it on port", "port", ios.HttpApiPort())
	ex, err := os.Executable()
	if err != nil {
		return fmt.Errorf("RunAgent: failed to get executable path: %w", err)
	}

	var cmd *exec.Cmd
	switch mode {
	case "kernel":
		cmd = exec.Command(ex, append([]string{"tunnel", "start"}, args...)...)
	case "user":
		cmd = exec.Command(ex, append([]string{"tunnel", "start", "--userspace"}, args...)...)
	default:
		return fmt.Errorf("RunAgent: unknown mode: %s. Only 'kernel' and 'user' are supported", mode)
	}

	// OS specific SysProcAttr assignment
	cmd.SysProcAttr = createSysProcAttr()

	err = cmd.Start()

	if err != nil {
		return fmt.Errorf("RunAgent: failed to start agent: %w", err)
	}
	err = cmd.Process.Release()
	if err != nil {
		return fmt.Errorf("RunAgent: failed to release process: %w", err)
	}
	WaitUntilAgentReady()
	return nil
}

// ServeTunnelInfo starts a simple http serve that exposes the tunnel information about the running tunnel.
// The API has two endpoints:
// 1. GET    localhost:{PORT}/tunnel/{UDID} to get the tunnel info for a specific device
// 2. DELETE localhost:{PORT}/tunnel/{UDID} to stop a device tunnel
// 3. GET    localhost:{PORT}/tunnels       to get a list of all tunnels
func ServeTunnelInfo(tm *TunnelManager, port int) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/ready", func(writer http.ResponseWriter, request *http.Request) {
		if tm.FirstUpdateCompleted() {
			writer.WriteHeader(http.StatusOK)
		} else {
			writer.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	mux.HandleFunc("/shutdown", func(writer http.ResponseWriter, request *http.Request) {
		err := tm.Close()
		if err != nil {
			log.Error("failed to close tunnel manager", err)
		}
		writer.WriteHeader(http.StatusOK)
		writer.Write([]byte("shutting down in 1 second..."))
		go func() {
			time.Sleep(1 * time.Second)
			os.Exit(0)
		}()
	})
	mux.HandleFunc("/tunnel/", func(writer http.ResponseWriter, request *http.Request) {
		udid := strings.TrimPrefix(request.URL.Path, "/tunnel/")
		if len(udid) == 0 {
			return
		}

		t, err := tm.FindTunnel(udid)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(t.Udid) == 0 {
			http.Error(writer, "", http.StatusNotFound)
			return
		}

		if request.Method == "GET" {
			writer.Header().Add("Content-Type", "application/json")
			enc := json.NewEncoder(writer)
			err = enc.Encode(t)
		} else if request.Method == "DELETE" {
			err = tm.stopTunnel(t)
		}
		if err != nil {
			http.Error(writer, err.Error(), http.StatusInternalServerError)
			return
		}
	})
	mux.HandleFunc("/tunnels", func(writer http.ResponseWriter, request *http.Request) {
		tunnels, err := tm.ListTunnels()
		if err != nil {
			http.Error(writer, err.Error(), http.StatusInternalServerError)
			return
		}

		writer.Header().Add("Content-Type", "application/json")
		enc := json.NewEncoder(writer)
		err = enc.Encode(tunnels)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusInternalServerError)
			return
		}
	})
	if err := http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", port), mux); err != nil {
		return fmt.Errorf("ServeTunnelInfo: failed to start http server: %w", err)
	}
	return nil
}

func TunnelInfoForDevice(udid string, tunnelInfoHost string, tunnelInfoPort int) (Tunnel, error) {
	c := http.Client{
		Timeout: 5 * time.Second,
	}
	res, err := c.Get(fmt.Sprintf("http://%s:%d/tunnel/%s", tunnelInfoHost, tunnelInfoPort, udid))
	if err != nil {
		return Tunnel{}, fmt.Errorf("TunnelInfoForDevice: failed to get tunnel info: %w", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return Tunnel{}, fmt.Errorf("TunnelInfoForDevice: failed to read body: %w", err)
	}
	var info Tunnel
	err = json.Unmarshal(body, &info)
	if err != nil {
		return Tunnel{}, fmt.Errorf("TunnelInfoForDevice: failed to parse response: %w", err)
	}
	return info, nil
}

func ListRunningTunnels(tunnelInfoHost string, tunnelInfoPort int) ([]Tunnel, error) {
	c := http.Client{
		Timeout: 5 * time.Second,
	}
	res, err := c.Get(fmt.Sprintf("http://%s:%d/tunnels", tunnelInfoHost, tunnelInfoPort))
	if err != nil {
		return nil, fmt.Errorf("TunnelInfoForDevice: failed to get tunnel info: %w", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("TunnelInfoForDevice: failed to read body: %w", err)
	}
	var info []Tunnel
	err = json.Unmarshal(body, &info)
	if err != nil {
		return nil, fmt.Errorf("TunnelInfoForDevice: failed to parse response: %w", err)
	}
	return info, nil
}

// TunnelManager starts tunnels for devices when needed (if no tunnel is running yet) and stores the information
// how those tunnels are reachable (address and remote service discovery port)
type TunnelManager struct {
	ts                   tunnelStarter
	dl                   deviceLister
	pm                   PairRecordManager
	mux                  sync.Mutex
	tunnels              map[string]Tunnel
	unsupportedDevices   map[string]struct{}
	startTunnelTimeout   time.Duration
	firstUpdateCompleted bool
	userspaceTUN         bool
	closeOnce            sync.Once
	portOffset           int
	rs                   remotedService
}

// NewTunnelManager creates a new TunnelManager instance for setting up device tunnels for all connected devices
// If userspaceTUN is set to true, the network stack will run in user space.
func NewTunnelManager(pm PairRecordManager, userspaceTUN bool) *TunnelManager {
	return &TunnelManager{
		ts:                 manualPairingTunnelStart{},
		dl:                 deviceList{},
		pm:                 pm,
		tunnels:            map[string]Tunnel{},
		unsupportedDevices: map[string]struct{}{},
		startTunnelTimeout: 10 * time.Second,
		userspaceTUN:       userspaceTUN,
		portOffset:         1,
		rs:                 NewRemotedService(),
	}
}

func (m *TunnelManager) Close() error {
	var baseErr error
	m.closeOnce.Do(func() {
		tunnels, err := m.ListTunnels()
		if err != nil {
			log.Error("failed to list tunnels", err)
		}
		for _, t := range tunnels {
			err := t.Close()
			baseErr = errors.Join(baseErr, err)
			if err != nil {
				log.WithField("udid", t.Udid).Error("failed to stop tunnel", err)
			}
		}
	})
	return baseErr
}

// FirstUpdateCompleted returns true if the first update completed,
// use it to prevent race conditions when trying to use go-ios agent for the first time
func (m *TunnelManager) FirstUpdateCompleted() bool {
	m.mux.Lock()
	defer m.mux.Unlock()
	return m.firstUpdateCompleted
}

// UpdateTunnels checks for connected devices and starts a new tunnel if needed
// On device disconnects the tunnel resources get cleaned up
func (m *TunnelManager) UpdateTunnels(ctx context.Context) error {
	m.mux.Lock()

	localTunnels := make(map[string]Tunnel, len(m.tunnels))
	for key, tunnel := range m.tunnels {
		localTunnels[key] = tunnel
	}

	localUnsupportedDevices := make(map[string]struct{}, len(m.unsupportedDevices))
	for key, udid := range m.unsupportedDevices {
		localUnsupportedDevices[key] = udid
	}

	m.mux.Unlock()

	// Get the list of current devices.
	devices, err := m.dl.ListDevices()
	if err != nil {
		return fmt.Errorf("UpdateTunnels: failed to get list of devices: %w", err)
	}

	usbmuxDevices := []ios.DeviceEntry{}
	usbncmDevices := []ios.DeviceEntry{}

	for _, d := range devices.DeviceList {
		// If this is a network device, ignore it (we don't track network vs. usb devices).
		if d.Properties.ConnectionType != "USB" {
			continue
		}

		udid := d.Properties.SerialNumber
		// If a tunnel already exists for this device, skip it.
		if _, exists := localTunnels[udid]; exists {
			continue
		}

		// If the device is in the unsupported list, skip it.
		if _, unsupported := localUnsupportedDevices[udid]; unsupported {
			continue
		}

		// Get the device version.
		version, err := ios.GetProductVersion(d)
		if err != nil {
			log.
				WithField("udid", udid).
				WithError(err).
				Error("startTunnel: failed to get device version")
			continue
		}

		if version.LessThan(semver.MustParse("17.0.0")) {
			// Check if this device is already in the unsupported list
			m.mux.Lock()
			if _, already := m.unsupportedDevices[udid]; already {
				m.mux.Unlock()
				continue
			}

			// Add to unsupported list
			m.unsupportedDevices[udid] = struct{}{}
			m.mux.Unlock()

			log.
				WithField("udid", udid).
				Tracef("skipping: unsupported iOS version %s", version.String())
			continue
		} else if version.LessThan(semver.MustParse("17.4.0")) {
			usbncmDevices = append(usbncmDevices, d)
		} else {
			usbmuxDevices = append(usbmuxDevices, d)
		}
	}

	var wg sync.WaitGroup

	// Create tunnels for USBMUX devices in parallel.
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.createUsbmuxTunnels(ctx, localTunnels, usbmuxDevices)
	}()

	// Create tunnels for USB NCM devices in parallel.
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.createUsbNcmTunnels(ctx, localTunnels, usbncmDevices)
	}()

	// Wait for both operations to complete.
	wg.Wait()

	// Remove tunnels for devices that are no longer present.
	m.removeDisconnectedTunnels(ctx, localTunnels, devices)

	// Mark the update as completed.
	m.mux.Lock()
	m.firstUpdateCompleted = true
	m.mux.Unlock()

	return nil
}

func (m *TunnelManager) createUsbmuxTunnels(ctx context.Context, localTunnels map[string]Tunnel, devices []ios.DeviceEntry) {
	var wg sync.WaitGroup
	for _, device := range devices {
		wg.Add(1)
		go func(device ios.DeviceEntry) {
			defer wg.Done()

			// If using userspace tunnel and the device port is unassigned, assign it.
			m.mux.Lock()
			if m.userspaceTUN && device.UserspaceTUNPort == 0 {
				device.UserspaceTUNPort = ios.HttpApiPort() + m.portOffset
				m.portOffset++
			}
			m.mux.Unlock()

			// Start the tunnel.
			t, err := m.startUsbmuxTunnel(ctx, device)
			if err != nil {
				log.WithField("udid", device.Properties.SerialNumber).
					WithError(err).
					Warn("failed to start tunnel")
				return
			}

			// Safely update the maps with the new tunnel.
			m.mux.Lock()
			localTunnels[device.Properties.SerialNumber] = t
			m.tunnels[device.Properties.SerialNumber] = t
			m.mux.Unlock()
		}(device)
	}
	wg.Wait()
}

func (m *TunnelManager) createUsbNcmTunnels(ctx context.Context, localTunnels map[string]Tunnel, devices []ios.DeviceEntry) {
	if len(devices) == 0 {
		return
	}

	// Find RSD service address.
	log.Info("looking for remoted services...")
	addrs, err := ios.FindRemotedServiceAddresses(ctx)
	if err != nil {
		log.WithError(err).Error("failed to find remoted services")
		return
	}
	log.Debugf("found %d possible interface(s)", len(addrs))

	resume_remoted, err := m.rs.suspendRemoted()
	if err != nil {
		log.WithError(err).
			Error("failed to suspend remoted")
	}

	deviceAddrList := []struct {
		Device  ios.DeviceEntry
		Address string
	}{}

	for _, addr := range addrs {
		log.Debugf("Checking interface: %s for a device", addr)
		udid, err := ios.TryGetRsdUdid(ctx, addr)
		if err != nil {
			log.WithError(err).
				WithField("addr", addr).
				Error("failed to get UDID from address")
			continue
		}

		log.Debugf("Found device: %s at address %s", udid, addr)

		// Check if the UDID exists in the list of USBNCM devices.
		var device ios.DeviceEntry
		found := false
		for _, d := range devices {
			if d.Properties.SerialNumber == udid {
				found = true
				device = d
				break
			}
		}
		if !found {
			log.Debugf("skipping tunnel to: %s as the device is not a USBNCM device.", udid)
			continue
		}

		deviceAddrList = append(deviceAddrList, struct {
			Device  ios.DeviceEntry
			Address string
		}{
			Device:  device,
			Address: addr,
		})
	}

	// Create go-routines to establish each tunnel
	var wg sync.WaitGroup
	for _, deviceAddress := range deviceAddrList {

		wg.Add(1)
		go func(device ios.DeviceEntry, addr string) {
			defer wg.Done()

			// Start the tunnel.
			t, err := m.startUsbNcmTunnel(ctx, device, addr)
			if err != nil {
				log.WithError(err).
					WithField("udid", device.Properties.SerialNumber).
					Warn("failed to start tunnel")
				return
			}

			// Safely update the maps with the new tunnel.
			m.mux.Lock()
			localTunnels[device.Properties.SerialNumber] = t
			m.tunnels[device.Properties.SerialNumber] = t
			m.mux.Unlock()
		}(deviceAddress.Device, deviceAddress.Address)
	}
	wg.Wait()

	resume_remoted()
}

func (m *TunnelManager) removeDisconnectedTunnels(ctx context.Context, localTunnels map[string]Tunnel, devices ios.DeviceList) {
	for udid, tunnel := range localTunnels {
		exists := false
		for _, d := range devices.DeviceList {
			if d.Properties.SerialNumber == udid && d.Properties.ConnectionType == "USB" {
				exists = true
				break
			}
		}

		if !exists || (tunnel.tunnelExited != nil && *tunnel.tunnelExited) {
			// Attempt to stop the tunnel.
			_ = m.stopTunnel(tunnel)

			// Remove the tunnel from the map.
			m.mux.Lock()
			delete(m.tunnels, udid)
			m.mux.Unlock()
		}
	}
}

func (m *TunnelManager) RemoveTunnel(ctx context.Context, serialNumber string) error {
	for udid, tun := range m.tunnels {
		if udid == serialNumber {
			err := m.stopTunnel(tun)
			return err
		}
	}

	return errors.New("tunnel not found")
}

func (m *TunnelManager) stopTunnel(t Tunnel) error {
	m.mux.Lock()
	defer m.mux.Unlock()
	log.WithField("udid", t.Udid).Info("stopping tunnel")
	delete(m.tunnels, t.Udid)

	return t.Close()
}

func (m *TunnelManager) startUsbmuxTunnel(ctx context.Context, device ios.DeviceEntry) (Tunnel, error) {
	log.WithField("udid", device.Properties.SerialNumber).Info("start tunnel")

	startTunnelCtx, cancel := context.WithTimeout(ctx, m.startTunnelTimeout)
	defer cancel()

	t, err := m.ts.StartUsbmuxTunnel(startTunnelCtx, device, m.userspaceTUN)
	if err != nil {
		return Tunnel{}, err
	}
	return t, nil
}

func (m *TunnelManager) startUsbNcmTunnel(ctx context.Context, device ios.DeviceEntry, addr string) (Tunnel, error) {
	log.WithField("udid", device.Properties.SerialNumber).Info("start tunnel")

	startTunnelCtx, cancel := context.WithTimeout(ctx, m.startTunnelTimeout)
	defer cancel()

	t, err := m.ts.StartUsbNcmTunnel(startTunnelCtx, device, m.pm, addr, m.rs)
	if err != nil {
		return Tunnel{}, err
	}
	return t, nil
}

// ListTunnels provides all currently running device tunnels
func (m *TunnelManager) ListTunnels() ([]Tunnel, error) {
	m.mux.Lock()
	defer m.mux.Unlock()
	return maps.Values(m.tunnels), nil
}

func (m *TunnelManager) FindTunnel(udid string) (Tunnel, error) {
	tunnels, err := m.ListTunnels()
	if err != nil {
		return Tunnel{}, err
	}

	for _, t := range tunnels {
		if t.Udid == udid {
			return t, nil
		}
	}

	return Tunnel{}, nil
}

type tunnelStarter interface {
	StartUsbmuxTunnel(ctx context.Context, device ios.DeviceEntry, userspaceTUN bool) (Tunnel, error)
	StartUsbNcmTunnel(ctx context.Context, device ios.DeviceEntry, p PairRecordManager, addr string, rs remotedService) (Tunnel, error)
}

type deviceLister interface {
	ListDevices() (ios.DeviceList, error)
}

type manualPairingTunnelStart struct {
}

func (m manualPairingTunnelStart) StartUsbmuxTunnel(ctx context.Context, device ios.DeviceEntry, userspaceTUN bool) (Tunnel, error) {

	if userspaceTUN {
		tun, err := ConnectUserSpaceTunnelLockdown(device, device.UserspaceTUNPort)
		tun.UserspaceTUN = true
		tun.UserspaceTUNPort = device.UserspaceTUNPort
		return tun, err
	}

	return ConnectTunnelLockdown(device)
}

func (m manualPairingTunnelStart) StartUsbNcmTunnel(ctx context.Context, device ios.DeviceEntry, p PairRecordManager, addr string, rs remotedService) (Tunnel, error) {
	return ManualPairAndConnectToTunnel(ctx, device, p, addr)
}

type deviceList struct {
}

func (d deviceList) ListDevices() (ios.DeviceList, error) {
	return ios.ListDevices()
}
