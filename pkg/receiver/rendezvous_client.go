package receiver

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/abferm/portal/models"
	"github.com/abferm/portal/models/protocol"
	"github.com/abferm/portal/pkg/crypt"
	"github.com/abferm/portal/tools"
	"github.com/gorilla/websocket"
	"github.com/schollz/pake/v3"
)

func (r *Receiver) ConnectToRendezvous(rendezvousAddress string, rendezvousPort int, password models.Password) (*websocket.Conn, error) {
	// establish websocket connection to rendezvous server
	rendezvousConn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s:%d/establish-receiver", rendezvousAddress, rendezvousPort), nil)
	if err != nil {
		return nil, err
	}
	err = r.establishSecureConnection(rendezvousConn, password)
	if err != nil {
		return nil, err
	}

	senderIP, senderPort, err := r.doTransferHandshake(rendezvousConn)
	if err != nil {
		return nil, err
	}

	directConn, err := r.probeSender(senderIP, senderPort)
	if err == nil {
		// notify sender through rendezvous that we will be using direct communication
		tools.WriteEncryptedMessage(rendezvousConn, protocol.TransferMessage{Type: protocol.ReceiverDirectCommunication}, r.crypt)
		// tell rendezvous to close the connection
		rendezvousConn.WriteJSON(protocol.RendezvousMessage{Type: protocol.ReceiverToRendezvousClose})
		return directConn, nil
	}

	r.usedRelay = true
	tools.WriteEncryptedMessage(rendezvousConn, protocol.TransferMessage{Type: protocol.ReceiverRelayCommunication}, r.crypt)

	transferMsg, err := tools.ReadEncryptedMessage(rendezvousConn, r.crypt)
	if err != nil {
		return nil, err
	}
	if transferMsg.Type != protocol.SenderRelayAck {
		return nil, err
	}

	return rendezvousConn, nil
}

func (r *Receiver) probeSender(senderIP net.IP, senderPort int) (*websocket.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	d := 250 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("could not establish a connection to the sender server")

		default:
			dialer := websocket.Dialer{HandshakeTimeout: d}
			wsConn, _, err := dialer.Dial(fmt.Sprintf("ws://%s:%d/portal", senderIP.String(), senderPort), nil)
			if err != nil {
				time.Sleep(d)
				d = d * 2
				continue
			}
			return wsConn, nil
		}
	}
}

func (r *Receiver) doTransferHandshake(wsConn *websocket.Conn) (net.IP, int, error) {
	tcpAddr, _ := wsConn.LocalAddr().(*net.TCPAddr)
	msg := protocol.TransferMessage{
		Type: protocol.ReceiverHandshake,
		Payload: protocol.ReceiverHandshakePayload{
			IP: tcpAddr.IP,
		},
	}

	err := tools.WriteEncryptedMessage(wsConn, msg, r.crypt)
	if err != nil {
		return nil, 0, err
	}

	msg, err = tools.ReadEncryptedMessage(wsConn, r.crypt)
	if err != nil {
		return nil, 0, err
	}

	if msg.Type != protocol.SenderHandshake {
		return nil, 0, protocol.NewWrongMessageTypeError([]protocol.TransferMessageType{protocol.SenderHandshake}, msg.Type)
	}

	handshakePayload := protocol.SenderHandshakePayload{}
	err = tools.DecodePayload(msg.Payload, &handshakePayload)
	if err != nil {
		return nil, 0, err
	}

	r.payloadSize = handshakePayload.PayloadSize

	return handshakePayload.IP, handshakePayload.Port, nil
}

func (r *Receiver) establishSecureConnection(wsConn *websocket.Conn, password models.Password) error {
	// init curve in background
	pakeCh := make(chan *pake.Pake)
	pakeErr := make(chan error)
	go func() {
		var err error
		p, err := pake.InitCurve([]byte(password), 1, "p256")
		pakeErr <- err
		pakeCh <- p
	}()

	wsConn.WriteJSON(protocol.RendezvousMessage{
		Type: protocol.ReceiverToRendezvousEstablish,
		Payload: protocol.PasswordPayload{
			Password: tools.HashPassword(password),
		},
	})

	msg, err := tools.ReadRendevouzMessage(wsConn, protocol.RendezvousToReceiverPAKE)
	if err != nil {
		return err
	}

	pakePayload := protocol.PakePayload{}
	err = tools.DecodePayload(msg.Payload, &pakePayload)
	if err != nil {
		return err
	}

	// check if we had an issue with the PAKE2 initialization error
	if err = <-pakeErr; err != nil {
		return err
	}

	p := <-pakeCh

	err = p.Update(pakePayload.Bytes)
	if err != nil {
		return err
	}

	wsConn.WriteJSON(protocol.RendezvousMessage{
		Type: protocol.ReceiverToRendezvousPAKE,
		Payload: protocol.PakePayload{
			Bytes: p.Bytes(),
		},
	})

	msg, err = tools.ReadRendevouzMessage(wsConn, protocol.RendezvousToReceiverSalt)
	if err != nil {
		return err
	}

	saltPayload := protocol.SaltPayload{}
	err = tools.DecodePayload(msg.Payload, &saltPayload)
	if err != nil {
		return err
	}

	sessionKey, err := p.SessionKey()
	if err != nil {
		return err
	}
	r.crypt, err = crypt.New(sessionKey, saltPayload.Salt)
	if err != nil {
		return err
	}
	return nil
}
