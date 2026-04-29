package protocol

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
)

// Message types
type MsgType string

const (
	TypeProbe     MsgType = "PROBE"
	TypeHandshake MsgType = "HANDSHAKE"
	TypeAck       MsgType = "ACK"
	TypeChat      MsgType = "CHAT"
	TypeCommand   MsgType = "CMD"
	TypeCmdAck    MsgType = "CMD_ACK"
	TypeFileHdr   MsgType = "FILE_HDR"
	TypeFileChunk MsgType = "FILE_CHUNK"
	TypeFileEnd   MsgType = "FILE_END"
	TypeHeartbeat MsgType = "HB"
	TypeBye       MsgType = "BYE"
	TypeState     MsgType = "STATE"
	TypeReport    MsgType = "REPORT"
	TypeRoster    MsgType = "ROSTER"
	TypeHandRaise MsgType = "HAND_RAISE"
	TypePin       MsgType = "PIN"
	TypeDelete    MsgType = "DELETE"
	TypeScreenshot MsgType = "SCREENSHOT"
	TypeScreenCast MsgType = "SCREEN_CAST"
)

// Command action strings
const (
	CmdLockScreen   = "lock_screen"
	CmdUnlockScreen = "unlock_screen"
	CmdShutdown     = "shutdown"
	CmdCloseApps    = "close_apps"
	CmdLaunchApp    = "launch_app"
	CmdFocusApp     = "focus_app"
	CmdMute         = "mute"
	CmdUnmute       = "unmute"
	CmdBlockChat    = "block_chat"
	CmdUnblockChat  = "unblock_chat"
	CmdBlockFiles   = "block_files"
	CmdUnblockFiles = "unblock_files"
	CmdBlockHands   = "block_hands"
	CmdHandsDown    = "hands_down"
	CmdStartMonitor = "start_monitor"
	CmdStopMonitor  = "stop_monitor"
	CmdRequestShot  = "request_shot"
	CmdClearChat    = "clear_chat"
	CmdPushOpen     = "push_open" // silently open a URL on students' machines
	CmdStartCast    = "start_cast"
	CmdStopCast     = "stop_cast"
)

// Wire format: newline-delimited JSON
// Each message is one JSON object terminated by \n

type Message struct {
	Type    MsgType         `json:"t"`
	ID      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"p,omitempty"`
}

// Payload types

type ProbePayload struct {
	ServerAddr string `json:"addr"` // teacher's main server host:port
}

type HandshakePayload struct {
	MAC      string `json:"mac"`
	Hostname string `json:"hostname"`
	Nickname string `json:"nickname"`
	Role     string `json:"role"`
}

type AckPayload struct {
	Accepted  bool   `json:"ok"`
	StudentID string `json:"sid"`
	Reason    string `json:"reason,omitempty"`
}

type ChatPayload struct {
	ID        string `json:"id,omitempty"` // preserved for history replay
	From      string `json:"from"`
	Content   string `json:"content"`
	Timestamp int64  `json:"ts"`
	Pinned    bool   `json:"pinned,omitempty"`
	FileID    string `json:"fid,omitempty"`
	FileName  string `json:"fname,omitempty"`
	FileSize  int64  `json:"fsize,omitempty"`
}

type CommandPayload struct {
	CmdID  string `json:"cid"`
	Target string `json:"target,omitempty"` // student ID or empty for broadcast
	Action string `json:"action"`
	Param  string `json:"param,omitempty"`
}

type CmdAckPayload struct {
	CmdID  string `json:"cid"`
	Action string `json:"action"`
	OK     bool   `json:"ok"`
	Error  string `json:"err,omitempty"`
}

type FileHdrPayload struct {
	FileID   string `json:"fid"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	MimeType string `json:"mime"`
	AutoOpen bool   `json:"auto_open,omitempty"` // open file immediately on receipt
}

type FileChunkPayload struct {
	FileID string `json:"fid"`
	Index  int    `json:"idx"`
	Data   []byte `json:"data"` // base64 encoded by json
}

type FileEndPayload struct {
	FileID string `json:"fid"`
}

type ScreenshotPayload struct {
	StudentID string `json:"sid"`
	Data      []byte `json:"data"` // JPEG bytes
}

type ScreenCastPayload struct {
	Data []byte `json:"data"` // JPEG bytes
}

type ReportPayload struct {
	MessageID string `json:"mid"`
	Content   string `json:"content"`
	Word      string `json:"word"`
	From      string `json:"from"`
}

type StatePayload struct {
	ChatBlocked  bool `json:"chat_blocked"`
	FilesBlocked bool `json:"files_blocked"`
	HandsBlocked bool `json:"hands_blocked"`
	ScreenLocked bool `json:"screen_locked"`
	Muted        bool `json:"muted"`
	Monitoring   bool `json:"monitoring"`
	Casting      bool `json:"casting,omitempty"`
}

type PinPayload struct {
	MsgID  string `json:"mid"`
	Pinned bool   `json:"pinned"`
}

// Conn wraps a net.Conn with buffered newline-delimited JSON read/write
type Conn struct {
	raw     net.Conn
	scanner *bufio.Scanner
}

func NewConn(c net.Conn) *Conn {
	s := bufio.NewScanner(c)
	s.Buffer(make([]byte, 4<<20), 4<<20) // 4MB max — covers file chunks
	return &Conn{raw: c, scanner: s}
}

func (c *Conn) Send(msg Message) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = c.raw.Write(b)
	return err
}

func (c *Conn) Recv() (Message, error) {
	var msg Message
	if !c.scanner.Scan() {
		err := c.scanner.Err()
		if err == nil {
			err = io.EOF
		}
		return msg, err
	}
	err := json.Unmarshal(c.scanner.Bytes(), &msg)
	return msg, err
}

func (c *Conn) Close() {
	c.raw.Close()
}

func (c *Conn) RemoteAddr() net.Addr {
	return c.raw.RemoteAddr()
}

func (c *Conn) Raw() net.Conn {
	return c.raw
}

// Encode builds a Message with a typed payload
func Encode(t MsgType, payload any) (Message, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return Message{}, fmt.Errorf("encode %s: %w", t, err)
	}
	return Message{Type: t, Payload: b}, nil
}

// Decode extracts a typed payload from a Message
func Decode[T any](msg Message) (T, error) {
	var v T
	err := json.Unmarshal(msg.Payload, &v)
	return v, err
}
