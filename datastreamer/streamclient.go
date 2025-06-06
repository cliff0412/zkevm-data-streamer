package datastreamer

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"

	"github.com/0xPolygonHermez/zkevm-data-streamer/log"
)

const (
	resultsBuffer  = 32  // Buffers for the results channel
	headersBuffer  = 32  // Buffers for the headers channel
	entriesBuffer  = 128 // Buffers for the entries channel
	entryRspBuffer = 32  // Buffers for data command response

	defaultTimeout = 5 * time.Second
)

// ProcessEntryFunc type of the callback function to process the received entry
type ProcessEntryFunc func(*FileEntry, *StreamClient, *StreamServer) error

// StreamClient type to manage a data stream client
type StreamClient struct {
	server       string // Server address to connect IP:port
	streamType   StreamType
	conn         net.Conn
	ID           string // Client id
	started      bool   // Flag client started
	connected    bool   // Flag client connected to server
	streaming    bool   // Flag client streaming started
	fromStream   uint64 // Start entry number from latest start command
	totalEntries uint64 // Total entries from latest header command

	results  chan ResultEntry // Channel to read command results
	headers  chan HeaderEntry // Channel to read header entries from the command Header
	entries  chan FileEntry   // Channel to read data entries from the streaming
	entryRsp chan FileEntry   // Channel to read data entries from the commands response

	nextEntry    uint64           // Next entry number to receive from streaming
	processEntry ProcessEntryFunc // Callback function to process the entry
	relayServer  *StreamServer    // Only used by the client on the stream relay server
}

// NewClient creates a new data stream client
func NewClient(server string, streamType StreamType) (*StreamClient, error) {
	// Create the client data stream
	c := StreamClient{
		server:       server,
		streamType:   streamType,
		ID:           "",
		started:      false,
		connected:    false,
		streaming:    false,
		fromStream:   0,
		totalEntries: 0,

		results:  make(chan ResultEntry, resultsBuffer),
		headers:  make(chan HeaderEntry, headersBuffer),
		entries:  make(chan FileEntry, entriesBuffer),
		entryRsp: make(chan FileEntry, entryRspBuffer),

		nextEntry:   0,
		relayServer: nil,
	}

	// Set default callback function to process entry
	c.setProcessEntryFunc(PrintReceivedEntry, c.relayServer)

	return &c, nil
}

// NewClientWithLogsConfig creates a new data stream client with logs configuration
func NewClientWithLogsConfig(server string, streamType StreamType, logsConfig log.Config) (*StreamClient, error) {
	log.Init(logsConfig)
	return NewClient(server, streamType)
}

// Start connects to the data stream server and starts getting data from the server
func (c *StreamClient) Start() error {
	// Connect to server
	c.connectServer()

	// Goroutine to read from the server all entry types
	go c.readEntries()

	// Goroutine to consume streaming entries
	go func() {
		err := c.getStreaming()
		if err != nil {
			log.Errorf("%s Error while getting streaming: %v", c.ID, err)
		}
	}()

	// Flag stared
	c.started = true

	return nil
}

// connectServer waits until the server connection is established and returns if a command result is pending
func (c *StreamClient) connectServer() bool {
	var err error

	// Connect to server
	for !c.connected {
		c.conn, err = net.Dial("tcp", c.server)
		if err != nil {
			log.Errorf("Error connecting to server %s: %v", c.server, err)
			time.Sleep(defaultTimeout)
			continue
		} else {
			// Connected
			c.connected = true
			c.ID = c.conn.LocalAddr().String()
			log.Infof("%s Connected to server: %s", c.ID, c.server)

			// Restore streaming
			if c.streaming {
				_, _, err = c.execCommand(CmdStart, true, c.nextEntry, nil)
				if err != nil {
					c.closeConnection()
					time.Sleep(defaultTimeout)
					continue
				}
				return true
			} else {
				return false
			}
		}
	}
	return false
}

// closeConnection closes connection to the server
func (c *StreamClient) closeConnection() {
	if c.conn != nil {
		log.Infof("%s Close connection", c.ID)
		c.conn.Close()
	}
	c.connected = false
}

// ExecCommandStart executes client TCP command to start streaming from entry
func (c *StreamClient) ExecCommandStart(fromEntry uint64) error {
	_, _, err := c.execCommand(CmdStart, false, fromEntry, nil)
	return err
}

// ExecCommandStartBookmark executes client TCP command to start streaming from bookmark
func (c *StreamClient) ExecCommandStartBookmark(fromBookmark []byte) error {
	_, _, err := c.execCommand(CmdStartBookmark, false, 0, fromBookmark)
	return err
}

// ExecCommandStop executes client TCP command to stop streaming
func (c *StreamClient) ExecCommandStop() error {
	_, _, err := c.execCommand(CmdStop, false, 0, nil)
	return err
}

// ExecCommandGetHeader executes client TCP command to get the header
func (c *StreamClient) ExecCommandGetHeader() (HeaderEntry, error) {
	header, _, err := c.execCommand(CmdHeader, false, 0, nil)
	return header, err
}

// ExecCommandGetEntry executes client TCP command to get an entry
func (c *StreamClient) ExecCommandGetEntry(fromEntry uint64) (FileEntry, error) {
	_, entry, err := c.execCommand(CmdEntry, false, fromEntry, nil)
	return entry, err
}

// ExecCommandGetBookmark executes client TCP command to get a bookmark
func (c *StreamClient) ExecCommandGetBookmark(fromBookmark []byte) (FileEntry, error) {
	_, entry, err := c.execCommand(CmdBookmark, false, 0, fromBookmark)
	return entry, err
}

// execCommand executes a valid client TCP command with deferred command result possibility
func (c *StreamClient) execCommand(cmd Command, deferredResult bool,
	fromEntry uint64, fromBookmark []byte) (HeaderEntry, FileEntry, error) {
	log.Debugf("%s Executing command %d[%s]...", c.ID, cmd, StrCommand[cmd])
	header := HeaderEntry{}
	entry := FileEntry{}

	// Check status of the client
	if !c.started {
		log.Errorf("Execute command not allowed. Client is not started")
		return header, entry, ErrExecCommandNotAllowed
	}

	// Check valid command
	if !cmd.IsACommand() {
		log.Errorf("%s Invalid command %d", c.ID, cmd)
		return header, entry, ErrInvalidCommand
	}

	// Send command
	err := writeFullUint64(uint64(cmd), c.conn)
	if err != nil {
		return header, entry, err
	}
	// Send stream type
	err = writeFullUint64(uint64(c.streamType), c.conn)
	if err != nil {
		return header, entry, err
	}

	// Send the command parameters
	switch cmd {
	case CmdStart:
		log.Debugf("%s ...from entry %d", c.ID, fromEntry)
		// Send starting/from entry number
		err = writeFullUint64(fromEntry, c.conn)
		if err != nil {
			return header, entry, err
		}
	case CmdStartBookmark:
		log.Debugf("%s ...from bookmark [%v]", c.ID, fromBookmark)
		// Send starting/from bookmark length
		err = writeFullUint32(uint32(len(fromBookmark)), c.conn)
		if err != nil {
			return header, entry, err
		}
		// Send starting/from bookmark
		err = writeFullBytes(fromBookmark, c.conn)
		if err != nil {
			return header, entry, err
		}
	case CmdEntry:
		log.Debugf("%s ...get entry %d", c.ID, fromEntry)
		// Send entry to retrieve
		err = writeFullUint64(fromEntry, c.conn)
		if err != nil {
			return header, entry, err
		}
	case CmdBookmark:
		log.Debugf("%s ...get bookmark [%v]", c.ID, fromBookmark)
		// Send bookmark length
		err = writeFullUint32(uint32(len(fromBookmark)), c.conn)
		if err != nil {
			return header, entry, err
		}
		// Send bookmark to retrieve
		err = writeFullBytes(fromBookmark, c.conn)
		if err != nil {
			return header, entry, err
		}
	}

	// Get the command result
	if !deferredResult {
		r := c.getResult(cmd)
		if r.errorNum != uint32(CmdErrOK) {
			return header, entry, ErrResultCommandError
		}
	}

	// Get the data response and update streaming flag
	switch cmd {
	case CmdStart:
		c.streaming = true
		c.fromStream = fromEntry
	case CmdStartBookmark:
		c.streaming = true
	case CmdStop:
		c.streaming = false
	case CmdHeader:
		h := c.getHeader()
		header = h
		c.totalEntries = header.TotalEntries
	case CmdEntry:
		e := c.getEntry()
		if e.Type == EntryTypeNotFound {
			return header, entry, ErrEntryNotFound
		}
		entry = e
	case CmdBookmark:
		e := c.getEntry()
		if e.Type == EntryTypeNotFound {
			return header, entry, ErrBookmarkNotFound
		}
		entry = e
	}

	return header, entry, nil
}

// writeFullUint64 writes to connection a complete uint64
func writeFullUint64(value uint64, conn net.Conn) error {
	buffer := make([]byte, 8) //nolint:mnd
	binary.BigEndian.PutUint64(buffer, value)

	var err error
	if conn != nil {
		_, err = conn.Write(buffer)
	} else {
		err = ErrNilConnection
	}
	if err != nil {
		log.Errorf("%s Error sending to server: %v", conn.RemoteAddr().String(), err)
		return err
	}
	return nil
}

// writeFullUint32 writes to connection a complete uint32
func writeFullUint32(value uint32, conn net.Conn) error {
	buffer := make([]byte, 4) //nolint:mnd
	binary.BigEndian.PutUint32(buffer, value)

	var err error
	if conn != nil {
		_, err = conn.Write(buffer)
	} else {
		err = ErrNilConnection
	}
	if err != nil {
		log.Errorf("%s Error sending to server: %v", conn.RemoteAddr().String(), err)
		return err
	}
	return nil
}

// writeFullBytes writes to connection the complete buffer
func writeFullBytes(buffer []byte, conn net.Conn) error {
	var err error
	if conn != nil {
		_, err = conn.Write(buffer)
	} else {
		err = ErrNilConnection
	}
	if err != nil {
		log.Errorf("%s Error sending to server: %v", conn.RemoteAddr().String(), err)
		return err
	}
	return nil
}

// readDataEntry reads bytes from server connection and returns a data entry type
func (c *StreamClient) readDataEntry() (FileEntry, error) {
	// Read the rest of fixed size fields
	buffer := make([]byte, FixedSizeFileEntry-1)
	err := c.readContent(buffer)
	if err != nil {
		return FileEntry{}, err
	}
	packet := []byte{PtData}
	buffer = append(packet, buffer...)

	// Read variable field (data)
	length := binary.BigEndian.Uint32(buffer[1:5])
	if length < FixedSizeFileEntry {
		log.Errorf("%s Error reading data entry", c.ID)
		return FileEntry{}, ErrReadingDataEntry
	}

	bufferAux := make([]byte, length-FixedSizeFileEntry)
	err = c.readContent(bufferAux)
	if err != nil {
		return FileEntry{}, err
	}
	buffer = append(buffer, bufferAux...) //nolint:makezero

	// Decode binary data to data entry struct
	d, err := DecodeBinaryToFileEntry(buffer)
	if err != nil {
		return d, err
	}

	return d, nil
}

// readHeaderEntry reads bytes from server connection and returns a header entry type
func (c *StreamClient) readHeaderEntry() (HeaderEntry, error) {
	h := HeaderEntry{}

	// Read the rest of header bytes
	buffer := make([]byte, headerSize-1)
	n, err := io.ReadFull(c.conn, buffer)
	if err != nil {
		log.Errorf("Error reading the header: %v", err)
		return h, err
	}
	if n != headerSize-1 {
		log.Error("Error getting header info")
		return h, ErrGettingHeaderInfo
	}
	packet := []byte{PtHeader}
	buffer = append(packet, buffer...)

	// Decode bytes stream to header entry struct
	h, err = decodeBinaryToHeaderEntry(buffer)
	if err != nil {
		log.Error("Error decoding binary header")
		return h, err
	}

	return h, nil
}

// readResultEntry reads bytes from server connection and returns a result entry type
func (c *StreamClient) readResultEntry() (ResultEntry, error) {
	// Read the rest of fixed size fields
	buffer := make([]byte, FixedSizeResultEntry-1)
	_, err := io.ReadFull(c.conn, buffer)
	if err != nil {
		if errors.Is(err, io.EOF) {
			log.Warnf("%s Server close connection", c.ID)
		} else {
			log.Errorf("%s Error reading from server: %v", c.ID, err)
		}
		return ResultEntry{}, err
	}
	packet := []byte{PtResult}
	buffer = append(packet, buffer...)

	// Read variable field (errStr)
	length := binary.BigEndian.Uint32(buffer[1:5])
	if length < FixedSizeResultEntry {
		log.Errorf("%s Error reading result entry", c.ID)
		return ResultEntry{}, ErrReadingResultEntry
	}

	bufferAux := make([]byte, length-FixedSizeResultEntry)
	err = c.readContent(bufferAux)
	if err != nil {
		return ResultEntry{}, err
	}
	buffer = append(buffer, bufferAux...) //nolint:makezero

	// Decode binary entry result
	e, err := DecodeBinaryToResultEntry(buffer)
	if err != nil {
		return e, err
	}
	// PrintResultEntry(e)
	return e, nil
}

// readContent reads raw content using the connection and places it into buffer parameter
func (c *StreamClient) readContent(buffer []byte) error {
	_, err := io.ReadFull(c.conn, buffer)
	if err != nil {
		if errors.Is(err, io.EOF) {
			log.Warnf("%s Server close connection", c.ID)
		} else {
			log.Errorf("%s Error reading from server: %w", c.ID, err)
		}
		return err
	}

	return nil
}

// readEntries reads from the server all type of packets
func (c *StreamClient) readEntries() {
	defer c.closeConnection()

	for {
		// Wait for connection
		deferredResult := c.connectServer()

		// Read packet type
		packet := make([]byte, 1)
		err := c.readContent(packet)
		if err != nil {
			c.closeConnection()
			continue
		}

		// Manage packet type
		switch packet[0] {
		case PtResult:
			// Read result entry data
			r, err := c.readResultEntry()
			if err != nil {
				c.closeConnection()
				continue
			}
			// Send data to results channel
			c.results <- r
			// Get the command deferred result
			if deferredResult {
				r := c.getResult(CmdStart)
				if r.errorNum != uint32(CmdErrOK) {
					c.closeConnection()
					time.Sleep(defaultTimeout)
					continue
				}
			}

		case PtDataRsp:
			// Read result entry data
			r, err := c.readDataEntry()
			if err != nil {
				c.closeConnection()
				continue
			}
			c.entryRsp <- r

		case PtHeader:
			// Read header entry data
			h, err := c.readHeaderEntry()
			if err != nil {
				c.closeConnection()
				continue
			}
			// Send data to headers channel
			c.headers <- h

		case PtData:
			// Read file/stream entry data
			e, err := c.readDataEntry()
			if err != nil {
				c.closeConnection()
				continue
			}
			// Send data to stream entries channel
			c.entries <- e

		default:
			// Unknown type
			log.Warnf("%s Unknown packet type %d", c.ID, packet[0])
			continue
		}
	}
}

// getResult consumes a result entry
func (c *StreamClient) getResult(cmd Command) ResultEntry {
	// Get result entry
	r := <-c.results
	log.Debugf("%s Result %d[%s] received for command %d[%s]", c.ID, r.errorNum, r.errorStr, cmd, StrCommand[cmd])
	return r
}

// getHeader consumes a header entry
func (c *StreamClient) getHeader() HeaderEntry {
	h := <-c.headers
	log.Debugf("%s Header received info: TotalEntries[%d], TotalLength[%d], Version[%d], SystemID[%d]",
		c.ID, h.TotalEntries, h.TotalLength, h.Version, h.SystemID)
	return h
}

// getEntry consumes a entry from commands response
func (c *StreamClient) getEntry() FileEntry {
	e := <-c.entryRsp
	log.Debugf("%s Entry received info: Number[%d]", c.ID, e.Number)
	return e
}

// getStreaming consumes streaming data entries
func (c *StreamClient) getStreaming() error {
	for {
		e := <-c.entries
		c.nextEntry = e.Number + 1

		// Process the data entry
		err := c.processEntry(&e, c, c.relayServer)
		if err != nil {
			log.Errorf("%s Processing entry %d: %s. Exiting getStream function", c.ID, e.Number, err.Error())
			return err
		}
	}
}

// GetFromStream returns streaming start entry number from the latest start command executed
func (c *StreamClient) GetFromStream() uint64 {
	return c.fromStream
}

// GetTotalEntries returns total entries number from the latest header command executed
func (c *StreamClient) GetTotalEntries() uint64 {
	return c.totalEntries
}

// SetProcessEntryFunc sets the callback function to process entry
func (c *StreamClient) SetProcessEntryFunc(f ProcessEntryFunc) {
	c.setProcessEntryFunc(f, nil)
}

// ResetProcessEntryFunc resets the callback function to the default one
func (c *StreamClient) ResetProcessEntryFunc() {
	// Set default callback function to process entry
	c.setProcessEntryFunc(PrintReceivedEntry, c.relayServer)
}

// setProcessEntryFunc sets the callback function to process entry with server parameter
func (c *StreamClient) setProcessEntryFunc(f ProcessEntryFunc, s *StreamServer) {
	c.processEntry = f
	c.relayServer = s
}

// IsStarted returns if the client is started
func (c *StreamClient) IsStarted() bool {
	return c.started
}

// PrintReceivedEntry prints received entry (default callback function)
func PrintReceivedEntry(e *FileEntry, c *StreamClient, s *StreamServer) error {
	// Log data entry fields
	log.Debugf("Data entry(%s): %d | %d | %d | %d", c.ID, e.Number, e.Length, e.Type, len(e.Data))
	return nil
}
