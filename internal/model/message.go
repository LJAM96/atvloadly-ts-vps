package model

// Message.Type
const (
	MessageTypeInstall = 1
	MessageType2FA     = 2

	MessageTypeLogin = 1

	MessageTypePair        = 1
	MessageTypePairConfirm = 2
	MessageTypePairDirect  = 3
)

// Message Websocket Communication data format
type Message struct {
	Type int    `json:"t"`
	Data string `json:"d"`
}
