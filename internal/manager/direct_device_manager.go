package manager

import (
	"github.com/bitxeno/atvloadly/internal/app"
	"github.com/bitxeno/atvloadly/internal/model"
)

func GetDirectDevices() []model.DirectDevice {
	if app.Settings == nil {
		return nil
	}

	return append([]model.DirectDevice(nil), app.Settings.Devices.Direct...)
}

func GetDirectDeviceByUDID(udid string) (*model.DirectDevice, bool) {
	for _, device := range GetDirectDevices() {
		if device.UDID == udid {
			d := device
			return &d, true
		}
	}

	return nil, false
}

func UpsertDirectDevice(device model.DirectDevice) {
	if app.Settings == nil || device.UDID == "" || device.RemoteIdentifier == "" || device.Host == "" {
		return
	}

	directDevices := GetDirectDevices()
	for i := range directDevices {
		if directDevices[i].UDID == device.UDID || directDevices[i].RemoteIdentifier == device.RemoteIdentifier {
			directDevices[i] = device
			app.Settings.Devices.Direct = directDevices
			app.SaveSettings()
			deviceManager.SaveDevice(device.ToDevice())
			return
		}
	}

	app.Settings.Devices.Direct = append(directDevices, device)
	app.SaveSettings()
	deviceManager.SaveDevice(device.ToDevice())
}
