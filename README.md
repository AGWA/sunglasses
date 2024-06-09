# Sunglasses - RFC6962 compatibility proxy for Sunlight logs

Sunglasses is a proxy server that presents an RFC6962-compatible view of a Sunlight log.

## Current Status

All RFC6962 endpoints are implemented, except that no certificate chain is returned in `extra_data`.  This will be fixed once the new version of the Sunlight protocol is deployed.

## Public Instances

These are for testing purposes only and should not be used in production.

* Rome 2024h1: `https://rome-2024h1.sunglasses.sslmate.net/`
* Sycamore 2024h2: `https://sycamore-2024h2.sunglasses.sslmate.net/`
* Twig 2025h1: `https://twig-2025h1.sunglasses.sslmate.net/`

## Installation

```
go install src.agwa.name/sunglasses@latest
```

## Command Line Arguments

### `-db PATH` (Mandatory)

Path to database file, which will be created if necessary.

### `-listen SOCKET` (Mandatory)

Listen on the given address, provided in [go-listener syntax](https://pkg.go.dev/src.agwa.name/go-listener#readme-listener-syntax).  You can specify the `-listen` flag multiple times to listen on multiple addresses.

### `-monitoring URL` (Mandatory)

URL prefix of the log's monitoring endpoint.

### `-submission URL` (Mandatory)

URL prefix of the log's submission endpoint.

## Example Usage

The following command will launch an RFC6962-compatible log at `https://rome-2024h1.sunglasses.example.com` which proxies requests to the Rome 2024h1 Sunlight log.

```
sunglasses -db /srv/sunglasses/rome-2024h1.db -listen tls:rome-2024h1.sunglasses.example.com:tcp:443 -monitoring https://rome2024h1.fly.storage.tigris.dev/ -submission https://rome.ct.filippo.io/2024h1/
```
