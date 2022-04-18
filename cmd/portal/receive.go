package main

import (
	"fmt"
	"math"
	"os"
	"time"

	"github.com/abferm/portal/constants"
	"github.com/abferm/portal/models"
	"github.com/abferm/portal/models/protocol"
	"github.com/abferm/portal/pkg/receiver"
	"github.com/abferm/portal/tools"
	"github.com/abferm/portal/ui"
	receiverui "github.com/abferm/portal/ui/receiver"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/gorilla/websocket"
)

// handleReceiveCommandis the receive application.
func handleReceiveCommand(programOptions models.ProgramOptions, password string) {
	// communicate ui updates on this channel between receiverClient and handleReceiveCmmand
	uiCh := make(chan receiver.UIUpdate)
	// initialize a receiverClient with a UI
	receiverClient := receiver.WithUI(receiver.NewReceiver(programOptions), uiCh)
	// initialize and start receiver-UI
	receiverUI := receiverui.NewReceiverUI()
	// clean up temporary files previously created by this command
	tools.RemoveTemporaryFiles(constants.RECEIVE_TEMP_FILE_NAME_PREFIX)

	go initReceiverUI(receiverUI)
	time.Sleep(ui.START_PERIOD)
	go listenForReceiverUIUpdates(receiverUI, uiCh)

	parsedPassword, err := tools.ParsePassword(password)
	if err != nil {
		receiverUI.Send(ui.ErrorMsg{Message: "Error parsing password, make sure you entered a correctly formatted password (e.g. 1-gamma-ray-quasar)."})
		ui.GracefulUIQuit(receiverUI)
	}

	// initiate communications with rendezvous-server
	wsConnCh := make(chan *websocket.Conn)
	go initiateReceiverRendezvousCommunication(receiverClient, receiverUI, parsedPassword, wsConnCh)

	// keeps program alive until finished
	doneCh := make(chan bool)
	// start receiving files
	go startReceiving(receiverClient, receiverUI, <-wsConnCh, doneCh)

	// wait for shut down to render final UI
	<-doneCh
	ui.GracefulUIQuit(receiverUI)
}

func initReceiverUI(receiverUI *tea.Program) {
	go func() {
		if err := receiverUI.Start(); err != nil {
			fmt.Println("Error initializing UI", err)
			os.Exit(1)
		}
		os.Exit(0)
	}()
}

func listenForReceiverUIUpdates(receiverUI *tea.Program, uiCh chan receiver.UIUpdate) {
	latestProgress := 0
	for uiUpdate := range uiCh {
		// limit progress update ui-send events
		newProgress := int(math.Ceil(100 * float64(uiUpdate.Progress)))
		if newProgress > latestProgress {
			latestProgress = newProgress
			receiverUI.Send(ui.ProgressMsg{Progress: uiUpdate.Progress})
		}
	}
}

func initiateReceiverRendezvousCommunication(receiverClient *receiver.Receiver, receiverUI *tea.Program, password models.Password, connectionCh chan *websocket.Conn) {
	wsConn, err := receiverClient.ConnectToRendezvous(receiverClient.RendezvousAddress(), receiverClient.RendezvousPort(), password)
	if err != nil {
		receiverUI.Send(ui.ErrorMsg{Message: "Something went wrong during connection-negotiation (did you enter the correct password?)"})
		ui.GracefulUIQuit(receiverUI)
	}
	receiverUI.Send(ui.FileInfoMsg{Bytes: receiverClient.PayloadSize()})
	connectionCh <- wsConn
}

func startReceiving(receiverClient *receiver.Receiver, receiverUI *tea.Program, wsConnection *websocket.Conn, doneCh chan bool) {
	tempFile, err := os.CreateTemp(os.TempDir(), constants.RECEIVE_TEMP_FILE_NAME_PREFIX)
	if err != nil {
		receiverUI.Send(ui.ErrorMsg{Message: "Something went wrong when creating the received file container."})
		ui.GracefulUIQuit(receiverUI)
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	// start receiving files from sender
	err = receiverClient.Receive(wsConnection, tempFile)
	if err != nil {
		receiverUI.Send(ui.ErrorMsg{Message: "Something went wrong during file transfer."})
		ui.GracefulUIQuit(receiverUI)
	}
	if receiverClient.UsedRelay() {
		wsConnection.WriteJSON(protocol.RendezvousMessage{Type: protocol.ReceiverToRendezvousClose})
	}

	// reset file position for reading
	tempFile.Seek(0, 0)

	// read received bytes from tmpFile
	receivedFileNames, decompressedSize, err := tools.DecompressAndUnarchiveBytes(tempFile)
	if err != nil {
		receiverUI.Send(ui.ErrorMsg{Message: "Something went wrong when expanding the received files."})
		ui.GracefulUIQuit(receiverUI)
	}
	receiverUI.Send(ui.FinishedMsg{Files: receivedFileNames, PayloadSize: decompressedSize})
	doneCh <- true
}
