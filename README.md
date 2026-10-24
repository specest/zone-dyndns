# zone-dyndns

Keeps DNS A-records at [Zone](https://zone.eu) (zone.ee / zone.eu) pointed at your dynamic
public IP. Go, one Docker container.

## How it works

Once every `CHECK_FREQUENCY` (5 minutes by default):

1. Read your public IP from `https://ifconfig.me/ip`.
2. Read the A-records of each zone named in `conf/records.conf` from the Zone API.
3. Repoint every listed record that points elsewhere. Create the listed records that are missing.

The live listing is the only state, so nothing cached can go stale. The tool adds records and
repoints them. It never deletes one, and it ignores records that `records.conf` does not name.

## Requirements

Docker with Compose v2 ([instructions](https://docs.docker.com/engine/install/)), a Zone
account with an [API key](https://help.zone.eu/en/kb/zone-api-en/), and a domain in that account.

## Install

```bash
git clone https://github.com/specest/zone-dyndns.git
cd zone-dyndns
cp .env.example .env && chmod 600 .env
cp conf/records.conf.example conf/records.conf
```

### .env

| Setting | Default | Meaning |
|---|---|---|
| `ZONE_USERNAME` | — | Zone username. Required. |
| `ZONE_API_KEY` | — | Zone API key. Required. |
| `CHECK_FREQUENCY` | `5` | Minutes between checks. |
| `NETWORK_RETRY_DELAY` | `10` | Seconds before retrying a failed operation. |
| `MAX_ATTEMPTS` | `5` | Tries per operation, then wait for the next check. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `TZ` | — | Time zone for log timestamps. Unset means UTC. |
| `PUID`, `PGID` | `65534` | Container user. Set only if a non-root account edits `conf/`. |

### conf/records.conf

One (sub)domain per line, `#` for comments:

```conf
# my home services
example.org
blog.example.org
```

Records are matched by name, so there are no resource numbers to look up. A leftover `=12323`
from an older version is ignored, and stripped when the tool tidies the file up.

## Run

```bash
docker compose up -d
docker compose logs -f
```

Startup logs one line. A check that changes nothing logs nothing. When an address changes you
get one line per record:

```
level=INFO msg="zone-dyndns starting" conf_dir=conf check_frequency=10m0s retry_delay=30s max_attempts=5 log_level=INFO dry_run=false
level=INFO msg="record updated" domain=example.org zone=example.org destination=203.0.113.7 previous=203.0.113.4
```

Set `LOG_LEVEL=debug` to see every check, including the records in your zone it deliberately
ignored. Logs rotate at 3 × 5 MB. The image has no shell: the log is your only window in.

No prebuilt image is published. Compose builds it locally, which takes a couple of minutes on
a Raspberry Pi.

## Check before it writes

```bash
docker compose run --rm zone-dyndns -dry-run
```

`-dry-run` reads the zone and logs every change it would make. It sends no mutating request
and writes no file, `records.conf` included.

| Flag | Effect |
|---|---|
| `-conf <dir>` | Read `records.conf` from another directory. Default `conf`. |
| `-dry-run` | Log the plan. Change nothing. |
| `-allow-empty-zone` | Allow creating the first record in a zone whose listing is empty. |

## Limitations

- **One A-record per name.** A name with two records (round-robin DNS) is left alone, and the
  log names both record IDs.
- **Records Zone marks `modify:false` are skipped.** Zone does not let them be changed.
- **A listing that cannot be read, or that comes back empty, is not treated as "record
  missing".** Nothing is created or changed in that zone. The first record in a genuinely new
  zone needs `-allow-empty-zone`.
- **Never deletes.** A domain you remove from `records.conf` keeps its DNS record.
- **IPv4 only**, and `ifconfig.me` must be reachable. If it answers with anything that is not
  a routable IPv4 address, the check is abandoned and DNS is left alone.

## Troubleshooting

| Log line | Cause |
|---|---|
| `Invalid auth credentials` (401) | Wrong `ZONE_USERNAME` or `ZONE_API_KEY`. |
| `Teenust ei leitud` (404) | Estonian for "service not found": the domain is not in this Zone account. |
| `no listing available for zone ...` | The API could not be read. Nothing was changed. |
| `could not tidy the domain list locally` | `conf/` is not writable by the container user: `chown -R <PUID>:<PGID> conf`. DNS was still updated. |

## Development

```bash
cd src && go test ./...    # 43 tests, no network access needed
```

Needs Go 1.27.3 or newer. Builds for linux/amd64, linux/arm64, darwin/arm64 and
windows/amd64. Shuts down on SIGTERM and Ctrl-C.

Upgrading: `conf/ip.conf` and `logs/` are no longer used, and can be deleted. `records.conf`
loses its record IDs on the first check that changes anything, comments kept. Keep `src/` —
the image builds from it.

A personal project, with no warranty.
