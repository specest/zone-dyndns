# zone-dyndns

Dynamic DNS for domains registered in zone.ee / zone.eu. This tool allows you to create and update DNS A-records at zone.ee / zone.eu domain name registrar using their API. 

Written in Go, runs in a docker container. 

Builds and runs fine on: 
* Fedora 38 - x86 
* MacOS - Apple M1 
* Debian Bookworm - Raspberry Pi 4

### Disclaimer

* This is a personal project quickly thrown together by someone who is not a professional programmer. Use with caution. 

* This has barely been tested. You might run into all sorts of trouble. Feel free to let me know, I might add a fix.

* You will probably run into trouble if you want to add more than ten A-records per root domain.


### Why should I use it?

You have a dynamic IP addres and have registered a domain at zone.ee and
* want to host something on your own infrastructure
* want to have VPN access to your (home) network
* ... (let me know!)

### Requirements

* Docker & docker compose [install instructions](https://docs.docker.com/engine/install/)
* Zone.ee account & [API key](https://help.zone.eu/en/kb/zone-api-en/), a domain registered at zone.ee

### How it works

The tool checks your public IP address (ipify API) every 5 minutes 
(this can be configured). It then compares the result to the 
IP address stored locally. If there is a mismatch, it updates all the
 A-records in zone.ee (domains listed in `conf/records.conf`) to 
 point to your new public IP-address. 

### How to use

Clone the repo and add .env and configuration files

```bash
git clone https://github.com/specest/zone-dyndns.git
cd zone-dyndns
cp ./.env.example ./.env
cp ./conf/ip.conf.example ./conf/ip.conf
cp ./conf/records.conf.example ./conf/records.conf
```

#### Configure .env
Add your zone.ee username and API key to .env file. 

By default the tool checks your public IP address every 5 minutes. You can change the frequency in .env file. 

#### Configure records.conf
Open `conf/records.conf` and add the domains and subdomains that 
you want to point to your IP-address.

Each A-record in zone.ee has a specific resource number, 
so you could add multiple A-records per domain. This tool 
only handles one record per domain, as it is unlikely that
anyone would want to have multiple A-records with a dynamic IP.
Feel free to clone and modify the code if this is what you need. 

```conf
# Enter domains with resource numbers you want to point to your own (dynamic) IP-address
# Each (sub)domain and resource id key-value pair on its own separate line
example.org=12323
# Domain without resource number
blog.example.org
```

If you don't know the resource id of your current A-record or don't have
any existing records for your domain(s), then worry not! The tool will
take care of finding the resource id for you. If it doesn't exist, it
will even create a new record for you! 

You might run into trouble if you already have more than one A-record 
pointing to a (sub)domain. In that case the easiest solution is to delete
all the records in zone.ee manually and let the tool recreate a new record.

If you have configured the .env and records.conf files, you are ready to 
#### Deploy the container 

Just run 
```bash
docker compose up -d
```
in project root directory. If you're running it on a raspberry pi or something similarly powered, it will take a few minutes to build and compile. 

You can verify with `docker ps` that the container is running. Logs are mounted to `logs/updater.log`. Check the logs to see if you have problems. 

If everything works, you're free to remove the `./src` directory, as this is no longer needed. 

```bash
rm -r src
```

If there are no (major) bugs, I might add a prebuilt docker container to a docker registry in the future for quicker deployment. Maybe prebuilt binaries as well, if there are more than two users of this tool.   