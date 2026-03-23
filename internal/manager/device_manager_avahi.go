//go:build linux

package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/bitxeno/atvloadly/internal/log"
	"github.com/bitxeno/atvloadly/internal/model"
	"github.com/bitxeno/atvloadly/internal/utils"
	gidevice "github.com/electricbubble/gidevice"
	"github.com/godbus/dbus/v5"
	"github.com/holoplot/go-avahi"
)

const (
	mdnsService         = "_apple-mobdev2._tcp"
	mdnsServicePairable = "_apple-pairable._tcp"
	mdnsServiceDomain   = "local"
	usbmuxdScanInterval = 10 * time.Second
)

var usbmux gidevice.Usbmux

// 需要依赖socket套接字：
// /var/run/dbus
// /var/run/avahi-daemon
func (dm *DeviceManager) Start() {
	dm.mu.Lock()
	dm.ctx, dm.cancel = context.WithCancel(context.Background())
	ctx := dm.ctx
	dm.mu.Unlock()

	dm.syncDirectDevices()
	dm.startUsbmuxdLoop(ctx)

	if err := dm.startAvahiDiscovery(ctx); err != nil {
		log.Warnf("Avahi discovery unavailable, continuing with usbmuxd-only mode: %v", err)
		<-ctx.Done()
	}
}

func (dm *DeviceManager) startUsbmuxdLoop(ctx context.Context) {
	umx, err := gidevice.NewUsbmux()
	if err != nil {
		log.Warnf("Cannot connect to usbmuxd, paired network devices will not be polled: %v", err)
		return
	}
	usbmux = umx

	go func() {
		dm.syncUsbmuxdNetworkDevices()

		ticker := time.NewTicker(usbmuxdScanInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				log.Info("usbmuxd device scanner stopped")
				return
			case <-ticker.C:
				dm.syncUsbmuxdNetworkDevices()
			}
		}
	}()
}

func (dm *DeviceManager) startAvahiDiscovery(ctx context.Context) error {
	conn, err := dbus.SystemBus()
	if err != nil {
		return fmt.Errorf("cannot get system bus: %w", err)
	}

	server, err := avahi.ServerNew(conn)
	if err != nil {
		return fmt.Errorf("avahi init failed: %w", err)
	}

	host, err := server.GetHostName()
	if err != nil {
		log.Err(err).Msgf("GetHostName() failed: ")
	}
	log.Tracef("GetHostName(): %s", host)

	fqdn, err := server.GetHostNameFqdn()
	if err != nil {
		log.Err(err).Msgf("GetHostNameFqdn() failed: ")
	}
	log.Tracef("GetHostNameFqdn(): %s", fqdn)

	s, err := server.GetAlternativeHostName(host)
	if err != nil {
		log.Err(err).Msgf("GetAlternativeHostName() failed: ")
	}
	log.Tracef("GetAlternativeHostName(): %s", s)

	i, err := server.GetAPIVersion()
	if err != nil {
		log.Err(err).Msgf("GetAPIVersion() failed: ")
	}
	log.Tracef("GetAPIVersion(): %v", i)

	hn, err := server.ResolveHostName(avahi.InterfaceUnspec, avahi.ProtoUnspec, fqdn, avahi.ProtoUnspec, 0)
	if err != nil {
		log.Err(err).Msgf("ResolveHostName() failed: ")
	}
	log.Tracef("ResolveHostName: %v", hn)

	sb, err := server.ServiceBrowserNew(avahi.InterfaceUnspec, avahi.ProtoUnspec, mdnsService, mdnsServiceDomain, 0)
	if err != nil {
		return fmt.Errorf("failed to browse %s: %w", mdnsService, err)
	}

	sbPairable, err := server.ServiceBrowserNew(avahi.InterfaceUnspec, avahi.ProtoUnspec, mdnsServicePairable, mdnsServiceDomain, 0)
	if err != nil {
		return fmt.Errorf("failed to browse %s: %w", mdnsServicePairable, err)
	}

	log.Info("Avahi discovery started...")

	var service avahi.Service

	for {
		select {
		case <-ctx.Done():
			log.Info("Avahi discovery stopped")
			return nil
		case service = <-sb.AddChannel:
			log.Tracef("ServiceBrowser ADD: %v", service)

			service, err := server.ResolveService(service.Interface, service.Protocol, service.Name,
				service.Type, service.Domain, avahi.ProtoUnspec, 0)
			if err == nil {
				log.Tracef(" RESOLVED >> %s", service.Address)

				macAddr := strings.Split(service.Name, "@")[0]
				name := dm.parseName(service.Host)
				// 检查是否已连接
				lockdownDevices, err := loadLockdownDevices()
				if err != nil {
					log.Err(err).Msg("loadLockdownDevices error: ")
					continue
				}
				log.Tracef("lockdown devices count >> %v", len(lockdownDevices))

				// 添加已连接设备，TODO：handshake检测是否可真实连接
				if lockdownDev, ok := lockdownDevices[macAddr]; ok {
					log.Tracef("add lockdown device >> %v", lockdownDev)
					udid := lockdownDev.Name
					device := model.Device{
						ID:          utils.Md5(udid),
						Name:        name,
						ServiceName: service.Name,
						MacAddr:     macAddr,
						IP:          service.Address,
						UDID:        udid,
						Source:      model.DeviceSourceAvahi,
						Status:      model.Paired,
					}
					device.ParseDeviceClass()

					dm.SaveDevice(device)

					// Trigger device connection callback
					dm.onDeviceConnected(device)
				}
			}
		case service = <-sb.RemoveChannel:
			log.Tracef("ServiceBrowser REMOVE: %v", service)
			macAddr := strings.Split(service.Name, "@")[0]
			dm.DeleteDeviceByMacAddr(macAddr)
		case service = <-sbPairable.AddChannel:
			log.Tracef("ServiceBrowser ADD: %v", service)

			service, err := server.ResolveService(service.Interface, service.Protocol, service.Name,
				service.Type, service.Domain, avahi.ProtoUnspec, 0)
			if err == nil {
				log.Tracef(" RESOLVED >> %s", service.Address)

				// 添加可配对设备
				macAddr := strings.Split(service.Name, "@")[0]
				name := dm.parseName(service.Host)
				udid := fmt.Sprintf("fff%sfff", macAddr)
				device := model.Device{
					ID:          utils.Md5(udid),
					Name:        name,
					ServiceName: service.Name,
					MacAddr:     macAddr,
					IP:          service.Address,
					UDID:        udid,
					Source:      model.DeviceSourceAvahi,
					Status:      model.Pairable,
				}
				device.ParseDeviceClass()
				dm.SaveDevice(device)

			}

		case service = <-sbPairable.RemoveChannel:
			log.Tracef("ServiceBrowser REMOVE: %v", service)
			macAddr := strings.Split(service.Name, "@")[0]
			udid := fmt.Sprintf("fff%sfff", macAddr)
			dm.DeleteDevice(udid)
		}
	}
}

func (dm *DeviceManager) Scan() {
	// TODO: AppleTV端删除连接后，本地自动删除已连接设备
}

func (dm *DeviceManager) syncUsbmuxdNetworkDevices() {
	dm.syncDirectDevices()
	devices, err := usbmux.Devices()
	if err != nil {
		log.Err(err).Msg("Cannot get devices from usbmuxd")
		return
	}

	keepConnectedDevices := make(map[string]bool)
	for _, d := range devices {
		if d.Properties().ConnectionType != "Network" {
			continue
		}

		udid := d.Properties().SerialNumber
		keepConnectedDevices[udid] = true
		macAddr := strings.Split(d.Properties().EscapedFullServiceName, "@")[0]

		device := model.Device{
			ID:          utils.Md5(udid),
			Name:        "AppleTV",
			ServiceName: d.Properties().EscapedFullServiceName,
			MacAddr:     macAddr,
			IP:          dm.parseNetworkAddress(d.Properties().NetworkAddress),
			UDID:        udid,
			Source:      model.DeviceSourceUsbmuxd,
		}

		if strings.Contains(udid, ":") {
			device.Status = model.Pairable
		} else {
			res, _ := d.GetValue("", "")
			data, _ := json.Marshal(res)
			devInfo := new(model.UsbmuxdDevice)
			if err := json.Unmarshal(data, devInfo); err == nil {
				device.Name = devInfo.DeviceName
				device.ProductType = devInfo.ProductType
				device.ProductVersion = devInfo.ProductVersion
				device.DeviceClass = devInfo.DeviceClass
			}
			device.Status = model.Paired
		}
		device.ParseDeviceClass()
		dm.SaveDevice(device)
	}

	dm.devices.Range(func(key, value any) bool {
		dev := value.(model.Device)
		if dev.Source == model.DeviceSourceUsbmuxd && !keepConnectedDevices[dev.UDID] {
			dm.devices.Delete(key)
		}
		return true
	})
}

func (dm *DeviceManager) parseNetworkAddress(networkAddress []byte) string {
	if len(networkAddress) == 0 {
		return ""
	}

	switch networkAddress[0] {
	case 16:
		if len(networkAddress) >= 8 {
			if ip, ok := netip.AddrFromSlice(networkAddress[4:8]); ok {
				return ip.String()
			}
		}
	case 28:
		if len(networkAddress) >= 32 {
			if ip, ok := netip.AddrFromSlice(networkAddress[16:32]); ok {
				return ip.String()
			}
		}
	}

	return ""
}

func (dm *DeviceManager) ScanServices(ctx context.Context, callback func(serviceType string, name string, host string, address string, port uint16, txt [][]byte)) error {
	conn, err := dbus.SystemBus()
	if err != nil {
		return fmt.Errorf("Cannot get system bus: %v", err)
	}

	server, err := avahi.ServerNew(conn)
	if err != nil {
		return fmt.Errorf("Avahi new failed: %v", err)
	}

	// Browse all service types
	stb, err := server.ServiceTypeBrowserNew(avahi.InterfaceUnspec, avahi.ProtoUnspec, mdnsServiceDomain, 0)
	if err != nil {
		return fmt.Errorf("ServiceTypeBrowserNew failed: %w", err)
	}

	discoveredTypes := make(map[string]bool)

	// Goroutine to handle type discovery and spawn service browsers
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-stb.AddChannel:
				if !discoveredTypes[t.Type] {
					discoveredTypes[t.Type] = true
					go dm.scanServiceTypeContinuous(ctx, server, t.Type, callback)
				}
			}
		}
	}()

	<-ctx.Done()
	return nil
}

func (dm *DeviceManager) scanServiceTypeContinuous(ctx context.Context, server *avahi.Server, serviceType string, callback func(serviceType string, name string, host string, address string, port uint16, txt [][]byte)) {
	sb, err := server.ServiceBrowserNew(avahi.InterfaceUnspec, avahi.ProtoUnspec, serviceType, mdnsServiceDomain, 0)
	if err != nil {
		log.Err(err).Msgf("ServiceBrowserNew failed for %s", serviceType)
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case service := <-sb.AddChannel:
			resolved, err := server.ResolveService(service.Interface, service.Protocol, service.Name,
				service.Type, service.Domain, avahi.ProtoUnspec, 0)
			if err == nil {
				callback(serviceType, resolved.Name, resolved.Host, resolved.Address, resolved.Port, resolved.Txt)
			}
		}
	}
}

func (dm *DeviceManager) ScanWirelessDevices(ctx context.Context, timeout time.Duration) ([]model.Device, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, fmt.Errorf("Cannot get system bus: %v", err)
	}

	server, err := avahi.ServerNew(conn)
	if err != nil {
		return nil, fmt.Errorf("Avahi new failed: %v", err)
	}

	sb, err := server.ServiceBrowserNew(avahi.InterfaceUnspec, avahi.ProtoUnspec, mdnsService, mdnsServiceDomain, 0)
	if err != nil {
		return nil, fmt.Errorf("ServiceBrowserNew() failed: %v", err)
	}

	devices := make([]model.Device, 0)
	deviceMap := make(map[string]bool)

	// 创建超时的context
	scanCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		select {
		case <-scanCtx.Done():
			return devices, nil
		case service := <-sb.AddChannel:
			resolved, err := server.ResolveService(service.Interface, service.Protocol, service.Name,
				service.Type, service.Domain, avahi.ProtoUnspec, 0)
			if err != nil {
				continue
			}

			macAddr := strings.Split(resolved.Name, "@")[0]
			// 避免重复添加
			if deviceMap[macAddr] {
				continue
			}
			deviceMap[macAddr] = true

			name := dm.parseName(resolved.Host)
			device := model.Device{
				ID:          utils.Md5(resolved.Name),
				Name:        name,
				ServiceName: service.Name,
				MacAddr:     macAddr,
				IP:          resolved.Address,
				Status:      model.Paired,
			}
			device.ParseDeviceClass()
			devices = append(devices, device)
		}
	}
}
