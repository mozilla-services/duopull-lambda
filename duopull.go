package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/kinesis"
	"github.com/aws/aws-sdk-go/service/s3"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	_ = iota
	debugOff
	debugDuo
	debugAWS
)

var debug = debugOff

// config is the global configuration structure for the function
type config struct {
	// Set from environment
	awsRegion        string // AWS region resources exist in
	awsS3Bucket      string // S3 bucket name for state
	awsKinesisStream string // Kinesis stream for event submission
	duoAPIHost       string // Duo API hostname
	duoIKey          string // Duo API ikey
	duoSKey          string // Duo API skey

	// Allocated during initialization
	awsS3Mintime string           // S3 bucket key name for mintime state
	awsSess      *session.Session // AWS session
	duo          *duoInterface    // Duo authorization header generator
}

// init loads configuration from the environment
func (c *config) init() error {
	c.awsRegion = os.Getenv("DUOPULL_REGION")
	c.awsS3Bucket = os.Getenv("DUOPULL_S3_BUCKET")
	c.awsKinesisStream = os.Getenv("DUOPULL_KINESIS_STREAM")
	c.duoAPIHost = os.Getenv("DUOPULL_HOST")
	c.duoIKey = os.Getenv("DUOPULL_IKEY")
	c.duoSKey = os.Getenv("DUOPULL_SKEY")

	c.awsS3Mintime = "mintime"

	err := c.validate()
	if err != nil {
		return err
	}

	if debug != debugAWS {
		c.duo = &duoInterface{
			apiHost: cfg.duoAPIHost,
			iKey:    cfg.duoIKey,
			sKey:    cfg.duoSKey,
		}
	}
	if debug != debugDuo {
		c.awsSess = session.Must(session.NewSession())
	}

	return nil
}

// validate verifies the config structure is valid given the operating mode
func (c *config) validate() error {
	if debug != debugDuo {
		if c.awsRegion == "" {
			return fmt.Errorf("DUOPULL_REGION must be set")
		}
		if c.awsS3Bucket == "" {
			return fmt.Errorf("DUOPULL_S3_BUCKET must be set")
		}
		if c.awsKinesisStream == "" {
			return fmt.Errorf("DUOPULL_KINESIS_STREAM must be set")
		}
	}
	if debug != debugAWS {
		if c.duoAPIHost == "" {
			return fmt.Errorf("DUOPULL_HOST must be set")
		}
		if c.duoIKey == "" {
			return fmt.Errorf("DUOPULL_IKEY must be set")
		}
		if c.duoSKey == "" {
			return fmt.Errorf("DUOPULL_SKEY must be set")
		}
	}
	return nil
}

var cfg config

// duoInterface is used to generate Authorization headers for requests to the Duo API
type duoInterface struct {
	apiHost string
	iKey    string
	sKey    string
}

// getAuthHeader returns an authentication header and date string header for use in a request
// to the Duo API.
func (d *duoInterface) getAuthHeader(method, path string, params map[string]string) (string, string) {
	ds := time.Now().UTC().Format("Mon, 2 Jan 2006 15:04:05 -0700")

	c := []string{
		ds,
		strings.ToUpper(method),
		strings.ToLower(d.apiHost),
		path,
	}
	paramval := url.Values{}
	for k, v := range params {
		paramval.Add(k, v)
	}
	c = append(c, paramval.Encode())
	template := strings.Join(c, "\n")

	h := hmac.New(sha1.New, []byte(d.sKey))
	h.Write([]byte(template))

	auth := fmt.Sprintf("%v:%v", d.iKey, hex.EncodeToString(h.Sum(nil)))
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(auth)), ds
}

// logRecords represents a response for a log request from the Duo API.
//
// The actual event information is treated as arbitrary JSON. The stat field is
// specifically included so we can inspect the value to confirm the request was
// successful.
//
// See also https://duo.com/docs/adminapi#api-details
type logRecords struct {
	Stat     string        `json:"stat"`
	Response []interface{} `json:"response"`
}

// emitEvent is an event which will be submitted to Kinesis
//
// Since the event itself contains no reference to the class of the event (e.g., authentication,
// administrator, etc) the path used to request the log is included here so the event types
// can be differentiated by the stream consumer.
type emitEvent struct {
	Path  string      `json:"path"`  // The request path (e.g., /api/v1/logs/telephony)
	Event interface{} `json:"event"` // The actual event
}

// getTimestamp extracts the timestamp value from e as an integer
func (e *emitEvent) getTimestamp() (int, error) {
	// Define a pseudo-struct for extraction of the timestamp instead of using
	// type assertions and dealing with float64 conversion
	type pse struct {
		Timestamp int `json:"timestamp"`
	}
	var p pse
	buf, err := json.Marshal(e.Event)
	if err != nil {
		return 0, err
	}
	err = json.Unmarshal(buf, &p)
	if err != nil {
		return 0, err
	}
	if p.Timestamp == 0 {
		return 0, fmt.Errorf("event had no timestamp")
	}
	return p.Timestamp, nil
}

// emitter stores all collected events for distribution to Kinesis
type emitter struct {
	events []emitEvent
}

// emit batches collected events to the configured Kinesis stream
func (e *emitter) emit() error {
	if debug == debugDuo { // Duo debug, just write events to stdout
		for _, v := range e.events {
			buf, err := json.Marshal(v)
			if err != nil {
				return err
			}
			fmt.Printf("%v\n", string(buf))
		}
		return nil
	}

	k := kinesis.New(cfg.awsSess, &aws.Config{
		Region: &cfg.awsRegion,
	})

	obuf := make([]*kinesis.PutRecordsRequestEntry, 0)
	for i, v := range e.events {
		e, err := json.Marshal(v)
		if err != nil {
			return err
		}
		obuf = append(obuf, &kinesis.PutRecordsRequestEntry{
			Data:         e,
			PartitionKey: aws.String("key"),
		})
		if i != 0 && len(obuf)%500 == 0 {
			_, err := k.PutRecords(&kinesis.PutRecordsInput{
				Records:    obuf,
				StreamName: &cfg.awsKinesisStream,
			})
			if err != nil {
				return err
			}
			obuf = obuf[:0]
		}
	}
	if len(obuf) != 0 {
		_, err := k.PutRecords(&kinesis.PutRecordsInput{
			Records:    obuf,
			StreamName: &cfg.awsKinesisStream,
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// minTime stores state related to the mintime parameter for the Duo API logging
// endpoints
type minTime struct {
	Administrator  int `json:"administrator"`  // mintime for administrator logs
	Authentication int `json:"authentication"` // mintime for authentication logs
	Telephony      int `json:"telephony"`      // mintime for telephony logs
}

// load pulls mintime state information from the S3 bucket
func (m *minTime) load() error {
	if debug == debugDuo {
		// Duo debug, just set an offset timestamp from current time for testing
		// purposes instead of loading it
		m.Administrator = int(time.Now().Add(-1 * (time.Minute * 60)).Unix())
		m.Authentication = m.Administrator
		m.Telephony = m.Administrator
		return nil
	}

	svc := s3.New(cfg.awsSess, &aws.Config{Region: &cfg.awsRegion})
	obj := &s3.GetObjectInput{Bucket: &cfg.awsS3Bucket, Key: &cfg.awsS3Mintime}

	result, err := svc.GetObject(obj)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			if aerr.Code() == "NoSuchKey" {
				// The mintime data was not found in S3; set a starting point 15
				// minutes in the past that will be used as an initial value
				m.Administrator = int(time.Now().Add(-1 * (time.Minute * 15)).Unix())
				m.Authentication = m.Administrator
				m.Telephony = m.Administrator
				return nil
			}
		}
		return err
	}

	buf, err := ioutil.ReadAll(result.Body)
	if err != nil {
		return err
	}
	err = json.Unmarshal(buf, m)
	if err != nil {
		return err
	}

	return nil
}

// save stores mintime state information
func (m *minTime) save() error {
	if debug == debugDuo { // Duo debug, noop
		return nil
	}

	svc := s3.New(cfg.awsSess, &aws.Config{Region: &cfg.awsRegion})
	buf, err := json.Marshal(m)
	if err != nil {
		return err
	}
	obj := &s3.PutObjectInput{
		Body:   strings.NewReader(string(buf)),
		Bucket: &cfg.awsS3Bucket,
		Key:    &cfg.awsS3Mintime,
	}
	_, err = svc.PutObject(obj)
	if err != nil {
		return err
	}
	return nil
}

// logRequest makes a request for logs from the Duo API from mintime onwards, using the
// specified API endpoint path for the request
func logRequest(d *duoInterface, mintime int, path string) ([]emitEvent, error) {
	mintimes := strconv.Itoa(mintime)

	if debug == debugAWS {
		// AWS debug, make an ad-hoc GET request to test outbound
		// connectivity and then just return a test event
		fmt.Println("making ad-hoc request")
		resp, err := http.Get("https://www.mozilla.org")
		if err != nil {
			return nil, err
		}
		fmt.Printf("ad-hoc request returned status code %v\n", resp.StatusCode)
		resp.Body.Close()
		return []emitEvent{
			{"/aws/test", map[string]interface{}{
				"aws":       "test",
				"timestamp": time.Now().Unix(),
			}},
		}, nil
	}

	req, err := http.NewRequest("GET", "https://"+d.apiHost+path, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	q.Add("mintime", mintimes)
	req.URL.RawQuery = q.Encode()

	authhdr, datehdr := d.getAuthHeader("GET", path, map[string]string{"mintime": mintimes})
	req.Header.Set("Authorization", authhdr)
	req.Header.Set("Date", datehdr)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%v returned code %v", path, resp.StatusCode)
	}
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var l logRecords
	err = json.Unmarshal(b, &l)
	if err != nil {
		return nil, err
	}
	if l.Stat != "OK" {
		return nil, fmt.Errorf("%v invalid stat, got %v", path, l.Stat)
	}
	ret := make([]emitEvent, 0)
	for _, v := range l.Response {
		ret = append(ret, emitEvent{Path: path, Event: v})
	}

	return ret, nil
}

// logRequestAdmin returns all administrator logs from the Duo API from mintime onwards
func logRequestAdmin(d *duoInterface, mintime int) ([]emitEvent, error) {
	return logRequest(d, mintime, "/admin/v1/logs/administrator")
}

// logRequestAuth returns all authentication logs from the Duo API from mintime onwards
func logRequestAuth(d *duoInterface, mintime int) ([]emitEvent, error) {
	return logRequest(d, mintime, "/admin/v1/logs/authentication")
}

// logRequestTele returns all telephony logs from the Duo API from mintime onwards
func logRequestTele(d *duoInterface, mintime int) ([]emitEvent, error) {
	return logRequest(d, mintime, "/admin/v1/logs/telephony")
}

// HandleRequest Lambda handler
func HandleRequest() error {
	var (
		m    minTime
		emit emitter
		err  error
	)

	err = cfg.init()
	if err != nil {
		return err
	}

	fmt.Println("loading mintime state")
	err = m.load()
	if err != nil {
		return err
	}

	// Define a helper function for extraction of the maximum timestamp from a
	// set of events returned from the API. If we get valid data back for a given
	// event type, the state will be adjusted so the next query starts from that
	// maximum event time + 1.
	//
	// See also https://duo.com/docs/adminapi#authentication-logs
	fh := func(es []emitEvent) (int, error) {
		var max int
		for _, x := range es {
			ts, err := x.getTimestamp()
			if err != nil {
				return 0, err
			}
			if ts > max {
				max = ts
			}
		}
		return max, nil
	}

	// Request administrator logs and adjust mintime
	fmt.Printf("requesting admin logs from %v\n", m.Administrator)
	e, err := logRequestAdmin(cfg.duo, m.Administrator)
	if err != nil {
		return err
	}
	nm, err := fh(e)
	if err != nil {
		return err
	}
	if nm != 0 {
		m.Administrator = nm + 1
	}
	emit.events = append(emit.events, e...)

	// Request authentication logs and adjust mintime
	fmt.Printf("requesting authentication logs from %v\n", m.Authentication)
	e, err = logRequestAuth(cfg.duo, m.Authentication)
	if err != nil {
		return err
	}
	nm, err = fh(e)
	if err != nil {
		return err
	}
	if nm != 0 {
		m.Authentication = nm + 1
	}
	emit.events = append(emit.events, e...)

	// Request telephony logs and adjust mintime
	fmt.Printf("requesting telephony logs from %v\n", m.Telephony)
	e, err = logRequestTele(cfg.duo, m.Telephony)
	if err != nil {
		return err
	}
	nm, err = fh(e)
	if err != nil {
		return err
	}
	if nm != 0 {
		m.Telephony = nm + 1
	}
	emit.events = append(emit.events, e...)

	fmt.Println("writing events")
	err = emit.emit()
	if err != nil {
		return err
	}

	fmt.Println("saving mintime state")
	err = m.save()
	if err != nil {
		return err
	}

	return nil
}

func main() {
	if os.Getenv("DEBUGDUO") == "1" {
		debug = debugDuo
	}
	if os.Getenv("DEBUGAWS") == "1" {
		if debug != debugOff {
			fmt.Fprint(os.Stderr, "DEBUGDUO and DEBUGAWS cannot both be set\n")
			os.Exit(1)
		}
		debug = debugAWS
	}
	if debug == debugDuo {
		// If DEBUGDUO is set, don't run as a Lambda function and instead just make
		// an ad-hoc request to the Duo API without utilizing any AWS resources
		err := HandleRequest()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	} else {
		lambda.Start(HandleRequest)
	}
}
