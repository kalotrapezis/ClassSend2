package network

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"sync"
	"time"

	"classsend/internal/protocol"
)

// Student represents a connected student on the teacher's server
type Student struct {
	ID       string
	MAC      string
	Hostname string
	Nickname string
	IP       string
	JoinedAt time.Time

	conn *protocol.Conn
}

func (s *Student) Send(msg protocol.Message) error {
	return s.conn.Send(msg)
}

// Server runs on the teacher's machine — accepts incoming student connections
type Server struct {
	mu       sync.RWMutex
	students map[string]*Student // keyed by student ID
	listener net.Listener

	OnJoin    func(*Student)
	OnLeave   func(*Student)
	OnMessage func(*Student, protocol.Message)
}

func NewServer() *Server {
	return &Server{
		students: make(map[string]*Student),
	}
}

func (s *Server) Start(port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("server listen on :%d: %w", port, err)
	}
	s.listener = ln
	go s.acceptLoop()
	return nil
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return // listener closed
		}
		go s.handleStudent(conn)
	}
}

func (s *Server) handleStudent(raw net.Conn) {
	raw.SetDeadline(time.Now().Add(10 * time.Second))
	conn := protocol.NewConn(raw)

	msg, err := conn.Recv()
	if err != nil || msg.Type != protocol.TypeHandshake {
		raw.Close()
		return
	}

	hs, err := protocol.Decode[protocol.HandshakePayload](msg)
	if err != nil {
		raw.Close()
		return
	}

	// MAC is the stable identity — fall back to random ID only if MAC is missing
	id := hs.MAC
	if id == "" {
		id = newID()
	}

	student := &Student{
		ID:       id,
		MAC:      hs.MAC,
		Hostname: hs.Hostname,
		Nickname: hs.Nickname,
		IP:       raw.RemoteAddr().(*net.TCPAddr).IP.String(),
		JoinedAt: time.Now(),
		conn:     conn,
	}

	ack, _ := protocol.Encode(protocol.TypeAck, protocol.AckPayload{
		Accepted:  true,
		StudentID: student.ID,
	})
	if err := conn.Send(ack); err != nil {
		raw.Close()
		return
	}

	// Clear deadline — connection is now persistent
	raw.SetDeadline(time.Time{})

	s.mu.Lock()
	// Evict stale connection for this MAC before registering the new one
	if old, exists := s.students[id]; exists {
		old.conn.Close()
	}
	s.students[student.ID] = student
	s.mu.Unlock()

	if s.OnJoin != nil {
		s.OnJoin(student)
	}

	// Read loop — blocks until student disconnects
	for {
		msg, err := conn.Recv()
		if err != nil {
			break
		}
		if s.OnMessage != nil {
			s.OnMessage(student, msg)
		}
	}

	s.mu.Lock()
	delete(s.students, student.ID)
	s.mu.Unlock()

	conn.Close()

	if s.OnLeave != nil {
		s.OnLeave(student)
	}
}

// Send sends a message to one student by ID. If the underlying write fails
// (timeout / broken pipe), the connection is closed so the read loop exits
// and the student is evicted from the map — otherwise subsequent Send calls
// would keep hitting the same dead conn.
func (s *Server) Send(studentID string, msg protocol.Message) error {
	s.mu.RLock()
	student, ok := s.students[studentID]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("student %s not connected", studentID)
	}
	if err := student.conn.Send(msg); err != nil {
		student.conn.Close()
		return err
	}
	return nil
}

// Broadcast sends a message to all connected students
func (s *Server) Broadcast(msg protocol.Message) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, st := range s.students {
		st.conn.Send(msg) // best-effort, ignore individual errors
	}
}

// Students returns a snapshot of all connected students
func (s *Server) Students() []*Student {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*Student, 0, len(s.students))
	for _, st := range s.students {
		result = append(result, st)
	}
	return result
}

// GetStudent returns a student by ID
func (s *Server) GetStudent(id string) (*Student, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.students[id]
	return st, ok
}

func (s *Server) Stop() {
	if s.listener != nil {
		s.listener.Close()
	}
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}
