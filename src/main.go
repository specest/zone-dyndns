package main

import (
	"io"
	"log"
	"os"
	"strconv"
	"time"
)

var USERNAME string = os.Getenv("USERNAME")
var PASSWORD string = os.Getenv("PASSWORD")
var NETWORK_RETRY_DELAY time.Duration // How long to wait before retrying on network error (seconds)
var CHECK_FREQUENCY time.Duration     // How often to check the IP (minutes)

func main() {
	envError := getEnv()

	for {
		l, err := os.OpenFile("logs/updater.log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {
			log.Fatalf("error opening file: %v", err)
		}
		mw := io.MultiWriter(os.Stdout, l)
		log.SetOutput(mw)

		// Check the environment variables error after setting log output to file
		if envError != nil {
			log.Fatalf("Error parsing environment variables. %v", envError)
		}
		domains := parseDomains()
		transactions := 0
		success := 0

		missing := getMissingResources(domains)
		getResourceIds(missing, domains)
		updateDomainList(domains)

		var publicIp string
		getPublicIp(&publicIp) // This has to be a pointer, otherwise retry on network failure won't work
		recordedIp := getRecordedIp()

		if recordedIp != publicIp {
			log.Println("Recorded IP and public IP mismatch, attempting to update DNS records.")
			transactions += len(domains) - len(missing)
			success += updateRecords(domains, publicIp)
		}

		if len(missing) > 0 {
			transactions += len(missing)
			success += createRecords(missing, domains, publicIp)
		}

		if success == transactions {
			updateIpRecord(publicIp)
			updateDomainList(domains)
		} else {
			updateDomainList(domains)
			log.Println("Something went wrong. Check the logs.", "Transactions:", transactions, "Successful:", success)
		}
		log.Printf("Finished updating records\n\n")
		l.Close()
		time.Sleep(CHECK_FREQUENCY * time.Minute)
	}
}

func getEnv() error {

	n := os.Getenv("NETWORK_RETRY_DELAY")
	nInt, err := strconv.Atoi(n)
	if err != nil {
		return err
	}
	NETWORK_RETRY_DELAY = time.Duration(nInt)

	c := os.Getenv("CHECK_FREQUENCY")
	cInt, err := strconv.Atoi(c)
	if err != nil {
		return err
	}
	CHECK_FREQUENCY = time.Duration(cInt)
	return nil
}
