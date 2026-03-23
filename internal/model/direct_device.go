package model

import "github.com/bitxeno/atvloadly/internal/utils"

type DirectDevice struct {
	Host             string `koanf:"host" json:"host"`
	Name             string `koanf:"name" json:"name"`
	UDID             string `koanf:"udid" json:"udid"`
	RemoteIdentifier string `koanf:"remote_identifier" json:"remote_identifier"`
	ManualPairPort   int    `koanf:"manual_pair_port" json:"manual_pair_port,omitempty"`
	TunnelPort       int    `koanf:"tunnel_port" json:"tunnel_port,omitempty"`
	DeviceClass      string `koanf:"device_class" json:"device_class,omitempty"`
	ProductType      string `koanf:"product_type" json:"product_type,omitempty"`
	ProductVersion   string `koanf:"product_version" json:"product_version,omitempty"`
}

func (d DirectDevice) ToDevice() Device {
	device := Device{
		ID:             utils.Md5(d.UDID),
		Name:           d.Name,
		IP:             d.Host,
		UDID:           d.UDID,
		Source:         DeviceSourceDirect,
		Status:         Paired,
		DeviceClass:    d.DeviceClass,
		ProductType:    d.ProductType,
		ProductVersion: d.ProductVersion,
	}
	device.ParseDeviceClass()
	return device
}
