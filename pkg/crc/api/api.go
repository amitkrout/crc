package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"

	"github.com/code-ready/crc/pkg/crc/machine"

	"github.com/code-ready/crc/pkg/crc/logging"
)

func CreateAPIServer(socketPath string, newConfig newConfigFunc, client machine.Client) (CrcAPIServer, error) {
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		logging.Error("Failed to create socket: ", err.Error())
		return CrcAPIServer{}, err
	}
	return createAPIServerWithListener(listener, newConfig, newHandler(client))
}

func createAPIServerWithListener(listener net.Listener, newConfig newConfigFunc, handler RequestHandler) (CrcAPIServer, error) {
	apiServer := CrcAPIServer{
		listener:               listener,
		newConfig:              newConfig,
		clusterOpsRequestsChan: make(chan clusterOpsRequest, 10),
		handler:                handler,
	}
	return apiServer, nil
}

func (api CrcAPIServer) Serve() {
	go api.handleClusterOperations() // go routine that handles start, stop and delete calls
	for {
		conn, err := api.listener.Accept()
		if err != nil {
			logging.Error("Error establishing communication: ", err.Error())
			continue
		}
		api.handleConnections(conn) // handle version, status, webconsole, etc. requests
	}
}

func (api CrcAPIServer) handleClusterOperations() {
	for req := range api.clusterOpsRequestsChan {
		api.handleRequest(req.command, req.socket)
	}
}

func (api CrcAPIServer) handleRequest(req commandRequest, conn net.Conn) {
	defer conn.Close()
	var result string

	config, err := api.newConfig()
	if err != nil {
		logging.Error(err.Error())
		result = encodeErrorToJSON(fmt.Sprintf("Failed to initialize new config store: %v", err))
		writeStringToSocket(conn, result)
		return
	}

	switch req.Command {
	case "start":
		result = api.handler.Start(config, req.Args)
	case "stop":
		result = api.handler.Stop()
	case "status":
		result = api.handler.Status()
	case "delete":
		result = api.handler.Delete()
	case "version":
		result = api.handler.GetVersion()
	case "setconfig":
		result = api.handler.SetConfig(config, req.Args)
	case "unsetconfig":
		result = api.handler.UnsetConfig(config, req.Args)
	case "getconfig":
		result = api.handler.GetConfig(config, req.Args)
	case "webconsoleurl":
		result = api.handler.GetWebconsoleInfo()
	default:
		result = encodeErrorToJSON(fmt.Sprintf("Unknown command supplied: %s", req.Command))
	}
	writeStringToSocket(conn, result)
}

func (api CrcAPIServer) handleConnections(conn net.Conn) {
	inBuffer := make([]byte, 1024)
	var req commandRequest
	numBytes, err := conn.Read(inBuffer)
	if err != nil || numBytes == 0 || numBytes == cap(inBuffer) {
		logging.Error("Error reading from socket")
		return
	}
	logging.Debug("Received Request:", string(inBuffer[0:numBytes]))
	err = json.Unmarshal(inBuffer[0:numBytes], &req)
	if err != nil {
		logging.Error("Error decoding request: ", err.Error())
		return
	}
	// start, stop and delete are slow operations, and change the VM state so they have to run sequentially.
	// We don't want other operations querying the status of the VM to be blocked by these,
	// so they are treated by a dedicated go routine

	switch req.Command {
	case "start", "stop", "delete":
		// queue new request to channel
		r := clusterOpsRequest{
			command: req,
			socket:  conn,
		}
		if !addRequestToChannel(r, api.clusterOpsRequestsChan) {
			logging.Error("Channel capacity reached, unable to add new request")
			errMsg := encodeErrorToJSON("Sockets channel capacity reached, unable to add new request")
			writeStringToSocket(conn, errMsg)
			conn.Close()
		}

	case "status", "version", "setconfig", "getconfig", "unsetconfig", "webconsoleurl":
		go api.handleRequest(req, conn)

	default:
		err := encodeErrorToJSON(fmt.Sprintf("Unknown command supplied: %s", req.Command))
		writeStringToSocket(conn, err)
		conn.Close()
	}
}

func writeStringToSocket(socket net.Conn, msg string) {
	var outBuffer bytes.Buffer
	_, err := outBuffer.WriteString(msg)
	if err != nil {
		logging.Error("Failed writing string to buffer", err.Error())
		return
	}
	_, err = socket.Write(outBuffer.Bytes())
	if err != nil {
		logging.Error("Failed writing string to socket", err.Error())
		return
	}
}

func addRequestToChannel(req clusterOpsRequest, requestsChan chan clusterOpsRequest) bool {
	select {
	case requestsChan <- req:
		return true
	default:
		return false
	}
}
