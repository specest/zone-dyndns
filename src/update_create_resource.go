package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

func updateRecords(domains map[string]string, publicIp string) int {
	log.Printf("Updating records for the following domains: %v", domains)
	success := 0
	for domain, id := range domains {
		if id == "" {
			continue
		}
		success += updateRecord(domain, id, publicIp)
	}
	return success
}

func updateRecord(domain, id, ip string) int {
	domainRoot := getDomainRoot(domain)
	url := fmt.Sprintf("https://api.zone.eu/v2/dns/%v/a/%v", domainRoot, id)
	var jsonStr = []byte(fmt.Sprintf(`{"destination": "%v", "name": "%v"}`, ip, domain))
	client := &http.Client{}
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewBuffer(jsonStr))
	req.Header.Set("Content-Type", "application/json")
	if err != nil {
		log.Printf("Updating DNS A-record for %v failed, error: %v", domain, err)
	}

	req.SetBasicAuth(USERNAME, PASSWORD)
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Error getting response from zone API: %v", err)
		log.Printf("Trying again in %v seconds", NETWORK_RETRY_DELAY)
		time.Sleep(time.Second * 15)
		return updateRecord(domain, id, ip)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read response body: %v", err)
		return updateRecord(domain, id, ip)
	}
	s := string(body)

	switch resp.StatusCode {
	case 200:
		log.Printf("Success: %v %v %v", resp.Status, domain, s)
	case 401:
		log.Printf("Failed to update resource. Unauthorized. Error: %v %v %v", resp.Status, domain, s)
		return 0
	case 404:
		log.Printf("Failed to update resource. Not found. Error: %v %v %v", resp.Status, domain, s)
		return 0
	case 422:
		log.Printf("Failed to update resource. Invalid input. Error: %v %v %v", resp.Status, domain, s)
		return 0
	default:
		log.Printf("Response:\nresource: %v\nbody: %v\nstatus: %v", resp.Status, domain, s)
		return 0
	}
	return 1
}

// Create new records and resource id's for new domains
func createRecords(missing, domains map[string]string, ip string) int {
	success := 0
	for domain := range missing {
		success += createRecord(domain, ip, domains)
	}
	return success
}

func createRecord(domain, ip string, domains map[string]string) int {
	domainRoot := getDomainRoot(domain)
	url := fmt.Sprintf("https://api.zone.eu/v2/dns/%v/a", domainRoot)
	var jsonStr = []byte(fmt.Sprintf(`{"destination": "%v", "name": "%v"}`, ip, domain))
	client := &http.Client{}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(jsonStr))
	req.Header.Set("Content-Type", "application/json")
	if err != nil {
		log.Printf("Creating DNS A-record for %v failed, error: %v", domain, err)
	}

	req.SetBasicAuth(USERNAME, PASSWORD)
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Error getting response from zone API: %v", err)
		log.Printf("Trying again in %v seconds", NETWORK_RETRY_DELAY)
		time.Sleep(time.Second * 15)
		return createRecord(domain, ip, domains)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read response body: %v", err)
		return createRecord(domain, ip, domains)
	}
	s := string(body)

	switch resp.StatusCode {
	case 201:
		log.Printf("Success: %v %v %v", domain, s, resp.Status)
		var r []struct {
			Name string `json:"name"`
			Id   string `json:"id"`
		}
		json.Unmarshal(body, &r)
		domains[domain] = r[0].Id
	case 401:
		log.Printf("Failed to create resource. Error: %v %v %v", resp.Status, domain, s)
		return 0
	case 404:
		log.Printf("Failed to create resource. Error: %v %v %v", resp.Status, domain, s)
		return 0
	case 402:
		log.Printf("Failed to create resource. Error: %v %v %v", resp.Status, domain, s)
		return 0
	case 422:
		log.Printf("Failed to create resource. Error: %v %v %v", resp.Status, domain, s)
		return 0
	default:
		log.Printf("Response:\nresource: %v\nbody: %v\nstatus: %v", resp.Status, domain, s)
		return 0
	}
	return 1
}
