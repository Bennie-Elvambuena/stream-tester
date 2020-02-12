// Package livepeer API
package livepeer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/golang/glog"
	"github.com/livepeer/stream-tester/internal/utils/uhttp"
	"github.com/livepeer/stream-tester/model"
)

const httpTimeout = 2 * time.Second

var httpClient = &http.Client{
	// Transport: &http2.Transport{TLSClientConfig: tlsConfig},
	Timeout: httpTimeout,
}

const (
	// ESHServer GCP? server
	ESHServer = "esh.livepeer.live"
	// ACServer Atlantic Crypto server
	ACServer = "chi.livepeer-ac.live"

	livepeerAPIGeolocateURL = "http://livepeer.live/api/geolocate"
)

type (
	// API object incapsulating Livepeer's hosted API
	API struct {
		choosenServer string
		accessToken   string
		presets       []string
	}

	geoResp struct {
		ChosenServer string `json:"chosenServer,omitempty"`
		Servers      []struct {
			Server   string `json:"server,omitempty"`
			Duration int    `json:"duration,omitempty"`
		} `json:"servers,omitempty"`
	}

	createStreamReq struct {
		Name    string   `json:"name,omitempty"`
		Presets []string `json:"presets,omitempty"`
		// one of
		// - P720p60fps16x9
		// - P720p30fps16x9
		// - P720p30fps4x3
		// - P576p30fps16x9
		// - P360p30fps16x9
		// - P360p30fps4x3
		// - P240p30fps16x9
		// - P240p30fps4x3
		// - P144p30fps16x9
		Profiles []struct {
			Name    string `json:"name,omitempty"`
			Width   int    `json:"width,omitempty"`
			Height  int    `json:"height,omitempty"`
			Bitrate int    `json:"bitrate,omitempty"`
			Fps     int    `json:"fps,omitempty"`
		} `json:"profiles,omitempty"`
	}

	createStreamResp struct {
		ID      string   `json:"id,omitempty"`
		Name    string   `json:"name,omitempty"`
		Presets []string `json:"presets,omitempty"`
		Kind    string   `json:"kind,omitempty"`
		UserID  string   `json:"userId,omitempty"`
		// renditions []struct
	}

	addressResp struct {
		Address string `json:"address,omitempty"`
	}
)

// NewLivepeer creates new Livepeer API object
func NewLivepeer(livepeerToken, serverOverride string, presets []string) *API {
	return &API{choosenServer: serverOverride, accessToken: livepeerToken, presets: presets}
}

// Init calles geolocation API endpoint to find closes server
// do nothing if `serverOverride` was not empty in the `NewLivepeer` call
func (lapi *API) Init() {
	if lapi.choosenServer != "" {
		return
	}

	resp, err := httpClient.Do(uhttp.GetRequest(livepeerAPIGeolocateURL))
	if err != nil {
		glog.Fatalf("Error geolocating Livpeer API server (%s) error: %v", livepeerAPIGeolocateURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		glog.Fatalf("Status error contacting Livpeer API server (%s) status %d body: %s", livepeerAPIGeolocateURL, resp.StatusCode, string(b))
	}
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		glog.Fatalf("Error geolocating Livpeer API server (%s) error: %v", livepeerAPIGeolocateURL, err)
	}
	glog.Info(string(b))
	geo := &geoResp{}
	err = json.Unmarshal(b, geo)
	if err != nil {
		panic(err)
	}
	glog.Infof("chosen server: %s, servers num: %d", geo.ChosenServer, len(geo.Servers))
	lapi.choosenServer = geo.ChosenServer
}

// Broadcasters returns list of hostnames of broadcasters to use
func (lapi *API) Broadcasters() ([]string, error) {
	u := fmt.Sprintf("https://%s/api/broadcaster", lapi.choosenServer)
	resp, err := httpClient.Do(uhttp.GetRequest(u))
	if err != nil {
		glog.Errorf("Error getting broadcasters from Livpeer API server (%s) error: %v", u, err)
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		glog.Fatalf("Status error contacting Livpeer API server (%s) status %d body: %s", livepeerAPIGeolocateURL, resp.StatusCode, string(b))
	}
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		glog.Fatalf("Error geolocating Livpeer API server (%s) error: %v", livepeerAPIGeolocateURL, err)
	}
	glog.Info(string(b))
	broadcasters := []addressResp{}
	err = json.Unmarshal(b, &broadcasters)
	if err != nil {
		return nil, err
	}
	bs := make([]string, 0, len(broadcasters))
	for _, a := range broadcasters {
		bs = append(bs, a.Address)
	}
	return bs, nil
}

// CreateStream creates stream with specified name and profiles
func (lapi *API) CreateStream(name string, profiles ...string) (string, error) {
	presets := profiles
	if len(presets) == 0 {
		presets = lapi.presets
	}
	glog.Infof("Creating Livepeer stream '%s' with profile '%v'", name, presets)
	reqs := &createStreamReq{
		Name:    name,
		Presets: presets,
	}
	b, err := json.Marshal(reqs)
	if err != nil {
		glog.V(model.SHORT).Infof("Error marshalling create stream request %v", err)
		return "", err
	}
	u := fmt.Sprintf("https://%s/api/stream", lapi.choosenServer)
	req, err := uhttp.NewRequest("POST", u, bytes.NewBuffer(b))
	if err != nil {
		return "", err
	}
	req.Header.Add("Authorization", "Bearer "+lapi.accessToken)
	req.Header.Add("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		glog.Errorf("Error creating Livepeer stream %v", err)
		return "", err
	}
	b, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		glog.Errorf("Error creating Livepeer stream (body) %v", err)
		return "", err
	}
	resp.Body.Close()
	glog.Info(string(b))
	r := &createStreamResp{}
	err = json.Unmarshal(b, r)
	if err != nil {
		return "", err
	}
	glog.Infof("Stream %s created with id %s", name, r.ID)
	return r.ID, nil
}

// DefaultPresets returns default presets
func (lapi *API) DefaultPresets() []string {
	return lapi.presets
}