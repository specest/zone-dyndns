package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

type Resources []struct {
	Id   string `json:"id"`
	Name string `json:"name"`
}

// Checks if all the (sub)domains have a resource identifier.
// Each record is identified by a string of numbers, i.e resource identifier. A domain can
// have multiple A records. If you are using this tool, you probably shouldn't have more than one.
func getMissingResources(domains map[string]string) map[string]string {
	missingResources := make(map[string]string)
	for domain, id := range domains {
		if id == "" {
			missingResources[domain] = ""
		}
	}
	return missingResources
}

// Get resource id's for the domains that are missing them
func getResourceIds(missing, domains map[string]string) int {
	success := 0
	for domain := range missing {
		getResourceId(missing, domains, domain)
	}
	return success
}

func getResourceId(missing, domains map[string]string, domain string) {
	domainRoot := getDomainRoot(domain)
	url := fmt.Sprintf("https://api.zone.eu/v2/dns/%v/a", domainRoot)
	client := &http.Client{}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		log.Printf("Getting DNS resource for %v failed, error: %v", domain, err)
	}

	req.SetBasicAuth(USERNAME, PASSWORD)
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Error getting response from zone API: %v", err)
		log.Printf("Trying again in %v seconds", NETWORK_RETRY_DELAY)
		time.Sleep(time.Second * NETWORK_RETRY_DELAY)
		getResourceId(missing, domains, domain)
		return

	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read response body: %v", err)
		time.Sleep(time.Second * NETWORK_RETRY_DELAY)
		getResourceId(missing, domains, domain)
		return
	}
	var r Resources
	json.Unmarshal(body, &r)
	for _, resource := range r {
		if resource.Name == domain {
			domains[domain] = resource.Id
			delete(missing, domain)
		}
	}
}
