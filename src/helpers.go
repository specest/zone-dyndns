package main

import (
	"bufio"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func getPublicIp(publicIp *string) {
	url := "https://api.ipify.org?format=text"
	log.Println("Getting IP address from ipify")
	resp, err := http.Get(url)
	if err != nil {
		log.Printf("Error getting response from ipify API: %v", err)
		log.Printf("Trying again in %v seconds", NETWORK_RETRY_DELAY)
		time.Sleep(time.Second * NETWORK_RETRY_DELAY)
		getPublicIp(publicIp)
		return
	}
	defer resp.Body.Close()
	ip, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Println(err)
		log.Printf("Trying again in %v seconds", NETWORK_RETRY_DELAY)
		time.Sleep(time.Second * NETWORK_RETRY_DELAY)
		getPublicIp(publicIp)
		return
	} else {
		log.Printf("Public IP is %v", string(ip))
	}
	*publicIp = strings.TrimSpace(string(ip))
}

func getRecordedIp() string {
	f, err := os.ReadFile("conf/ip.conf")
	if err != nil {
		log.Fatalf("unable to read file: %v", err)
	}

	ip := strings.TrimSpace(string(f))
	if ip == "" {
		ip = "missing"
	}
	log.Printf("Current IP-address on record is %v\n", ip)

	return ip
}

func parseDomains() map[string]string {
	domains := make(map[string]string)
	f, err := os.Open("conf/records.conf")
	if err != nil {
		log.Fatalf("error opening file: %v, unable to open domain list", err)
	}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || string(line[0]) == "#" {
			continue
		}
		s := strings.Split(line, "=")
		if len(s) > 1 {
			domains[s[0]] = s[1]
		} else {
			domains[s[0]] = ""
		}
	}
	if err := scanner.Err(); err != nil {
		log.Fatal(err)
	}
	return domains
}

func getDomainRoot(domain string) string {
	s := strings.Split(domain, ".")
	return s[len(s)-2] + "." + s[len(s)-1]
}

func updateIpRecord(ip string) {
	err := os.WriteFile("conf/ip.conf", []byte(ip), 0660)
	if err != nil {
		log.Fatalf("unable to update local IP record: %v", err)
	}
}

func updateDomainList(domains map[string]string) {
	var s string
	s += recordsComment
	for domain, id := range domains {
		s += domain + "=" + id + "\n"
	}
	err := os.WriteFile("conf/records.conf", []byte(s), 0660)
	if err != nil {
		log.Fatalf("unable to update local IP record: %v", err)
	}
}

var recordsComment string = `# Enter (sub)domains with resource numbers you want to point to your own (dynamic) IP-address
# Each (sub)domain and resource number key-value pair on its own separate line
# example.org=12323
# blog.example.org=146532
# example.com=112122` + "\n"
