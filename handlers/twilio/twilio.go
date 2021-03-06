package twilio

/*
 * Handler for Twilio channels, see https://www.twilio.com/docs/api
 */

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/buger/jsonparser"

	"github.com/nyaruka/courier"
	"github.com/nyaruka/courier/handlers"
	"github.com/nyaruka/courier/utils"
	"github.com/pkg/errors"
)

// TODO: agree on case!
const configAccountSID = "ACCOUNT_SID"
const configMessagingServiceSID = "messaging_service_sid"
const configSendURL = "send_url"

const twSignatureHeader = "X-Twilio-Signature"

var sendURL = "https://api.twilio.com/2010-04-01/Accounts"

// error code twilio returns when a contact has sent "stop"
const errorStopped = 21610

type handler struct {
	handlers.BaseHandler
}

// NewHandler returns a new TwilioHandler ready to be registered
func NewHandler() courier.ChannelHandler {
	return &handler{handlers.NewBaseHandler(courier.ChannelType("TW"), "Twilio")}
}

func init() {
	courier.RegisterHandler(NewHandler())
}

// Initialize is called by the engine once everything is loaded
func (h *handler) Initialize(s courier.Server) error {
	h.SetServer(s)
	err := s.AddReceiveMsgRoute(h, "POST", "receive", h.ReceiveMessage)
	if err != nil {
		return err
	}

	return s.AddUpdateStatusRoute(h, "POST", "status", h.StatusMessage)
}

type twMessage struct {
	MessageSID  string `validate:"required"`
	AccountSID  string `validate:"required"`
	From        string `validate:"required"`
	FromCountry string
	To          string `validate:"required"`
	ToCountry   string
	Body        string `validate:"required"`
	NumMedia    int
}

type twStatus struct {
	MessageSID    string `validate:"required"`
	MessageStatus string `validate:"required"`
	ErrorCode     string
}

var twStatusMapping = map[string]courier.MsgStatusValue{
	"queued":      courier.MsgSent,
	"failed":      courier.MsgFailed,
	"sent":        courier.MsgSent,
	"delivered":   courier.MsgDelivered,
	"undelivered": courier.MsgFailed,
}

// ReceiveMessage is our HTTP handler function for incoming messages
func (h *handler) ReceiveMessage(channel courier.Channel, w http.ResponseWriter, r *http.Request) ([]courier.Msg, error) {
	err := h.validateSignature(channel, r)
	if err != nil {
		return nil, err
	}

	// get our params
	twMsg := &twMessage{}
	err = handlers.DecodeAndValidateForm(twMsg, r)
	if err != nil {
		return nil, err
	}

	// create our URN
	urn := courier.NewTelURNForCountry(twMsg.From, twMsg.FromCountry)

	if twMsg.Body != "" {
		// Twilio sometimes sends concatenated sms as base64 encoded MMS
		twMsg.Body = handlers.DecodePossibleBase64(twMsg.Body)
	}

	// build our msg
	msg := h.Backend().NewIncomingMsg(channel, urn, twMsg.Body).WithExternalID(twMsg.MessageSID)

	// process any attached media
	for i := 0; i < twMsg.NumMedia; i++ {
		mediaURL := r.PostForm.Get(fmt.Sprintf("MediaUrl%d", i))
		msg.WithAttachment(mediaURL)
	}

	// and finally queue our message
	err = h.Backend().WriteMsg(msg)
	if err != nil {
		return nil, err
	}

	return []courier.Msg{msg}, h.writeReceiveSuccess(w)
}

// StatusMessage is our HTTP handler function for status updates
func (h *handler) StatusMessage(channel courier.Channel, w http.ResponseWriter, r *http.Request) ([]courier.MsgStatus, error) {
	err := h.validateSignature(channel, r)
	if err != nil {
		return nil, err
	}

	// get our params
	twStatus := &twStatus{}
	err = handlers.DecodeAndValidateForm(twStatus, r)
	if err != nil {
		return nil, err
	}

	msgStatus, found := twStatusMapping[twStatus.MessageStatus]
	if !found {
		return nil, fmt.Errorf("unknown status '%s', must be one of 'queued', 'failed', 'sent', 'delivered', or 'undelivered'", twStatus.MessageStatus)
	}

	// write our status
	status := h.Backend().NewMsgStatusForExternalID(channel, twStatus.MessageSID, msgStatus)
	err = h.Backend().WriteMsgStatus(status)
	if err != nil {
		return nil, err
	}

	return []courier.MsgStatus{status}, courier.WriteStatusSuccess(w, r, status)
}

// SendMsg sends the passed in message, returning any error
func (h *handler) SendMsg(msg courier.Msg) (courier.MsgStatus, error) {
	// build our callback URL
	callbackURL := fmt.Sprintf("%s/c/kn/%s/status/", h.Server().Config().BaseURL, msg.Channel().UUID())

	accountSID := msg.Channel().StringConfigForKey(configAccountSID, "")
	if accountSID == "" {
		return nil, fmt.Errorf("missing account sid for twilio channel")
	}

	accountToken := msg.Channel().StringConfigForKey(courier.ConfigAuthToken, "")
	if accountToken == "" {
		return nil, fmt.Errorf("missing account auth token for twilio channel")
	}

	// build our request
	form := url.Values{
		"To":             []string{msg.URN().Path()},
		"Body":           []string{msg.Text()},
		"StatusCallback": []string{callbackURL},
	}

	// add any media URL
	if len(msg.Attachments()) > 0 {
		_, mediaURL := courier.SplitAttachment(msg.Attachments()[0])
		form["MediaURL"] = []string{mediaURL}
	}

	// set our from, either as a messaging service or from our address
	serviceSID := msg.Channel().StringConfigForKey(configMessagingServiceSID, "")
	if serviceSID != "" {
		form["MessagingServiceSID"] = []string{serviceSID}
	} else {
		form["From"] = []string{msg.Channel().Address()}
	}

	baseSendURL := msg.Channel().StringConfigForKey(configSendURL, sendURL)
	sendURL, err := utils.AddURLPath(baseSendURL, accountSID, "Messages.json")
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, sendURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	rr, err := utils.MakeHTTPRequest(req)

	// record our status and log
	status := h.Backend().NewMsgStatusForID(msg.Channel(), msg.ID(), courier.MsgErrored)
	log := courier.NewChannelLogFromRR("Message Sent", msg.Channel(), msg.ID(), rr).WithError("Message Send Error", err)
	status.AddLog(log)

	// fail if we received an error
	if err != nil {
		return status, nil
	}

	// was this request successful?
	errorCode, _ := jsonparser.GetInt([]byte(rr.Body), "error_code")
	if errorCode != 0 {
		if errorCode == errorStopped {
			status.SetStatus(courier.MsgFailed)
			h.Backend().StopMsgContact(msg)
		}
		log.WithError("Message Send Error", errors.Errorf("received error code from twilio '%d'", errorCode))
		return status, nil
	}

	// grab the external id
	externalID, err := jsonparser.GetString([]byte(rr.Body), "sid")
	if err != nil {
		log.WithError("Message Send Error", errors.Errorf("unable to get sid from body"))
		return status, nil
	}

	status.SetStatus(courier.MsgWired)
	status.SetExternalID(externalID)

	return status, nil
}

// Twilio expects Twiml from a message receive request
func (h *handler) writeReceiveSuccess(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(200)
	_, err := fmt.Fprint(w, "<Response/>")
	return err
}

// see https://www.twilio.com/docs/api/security
func (h *handler) validateSignature(channel courier.Channel, r *http.Request) error {
	if err := r.ParseForm(); err != nil {
		return err
	}

	url := fmt.Sprintf("%s%s", h.Server().Config().BaseURL, r.URL.RequestURI())
	confAuth := channel.ConfigForKey(courier.ConfigAuthToken, "")
	authToken, isStr := confAuth.(string)
	if !isStr || authToken == "" {
		return fmt.Errorf("invalid or missing auth token in config")
	}

	expected, err := twCalculateSignature(url, r.PostForm, authToken)
	if err != nil {
		return err
	}

	actual := r.Header.Get(twSignatureHeader)
	if actual == "" {
		return fmt.Errorf("missing request signature")
	}

	// compare signatures in way that isn't sensitive to a timing attack
	if !hmac.Equal(expected, []byte(actual)) {
		return fmt.Errorf("invalid request signature")
	}
	return nil
}

// see https://www.twilio.com/docs/api/security
func twCalculateSignature(url string, form url.Values, authToken string) ([]byte, error) {
	var buffer bytes.Buffer
	buffer.WriteString(url)

	keys := make(sort.StringSlice, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	keys.Sort()

	for _, k := range keys {
		buffer.WriteString(k)
		for _, v := range form[k] {
			buffer.WriteString(v)
		}
	}

	// hash with SHA1
	mac := hmac.New(sha1.New, []byte(authToken))
	mac.Write(buffer.Bytes())
	hash := mac.Sum(nil)

	// encode with Base64
	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(hash)))
	base64.StdEncoding.Encode(encoded, hash)

	return encoded, nil
}
