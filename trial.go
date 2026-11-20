package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

func runTrial(origin string, port int) error {
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	body := encode(args{"port": port})
	registered := false
	for i := 0; i < 20; i++ {
		response, err := client.Post(origin+"/register", "application/json", bytes.NewReader(body))
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 65536))
			_ = response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				registered = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !registered {
		return errors.New("Trial registration failed")
	}
	for {
		response, err := client.Get(origin + "/done")
		if err == nil {
			var status struct {
				Done bool `json:"done"`
			}
			err = json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&status)
			_ = response.Body.Close()
			if response.StatusCode == 200 && err == nil && status.Done {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
}
