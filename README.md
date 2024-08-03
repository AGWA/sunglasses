# Sunglasses - RFC 6962 compatibility proxy for static-ct-api logs

Sunglasses is a proxy server that presents an RFC 6962-compatible view of a [static-ct-api](https://github.com/C2SP/C2SP/blob/main/static-ct-api.md) log.

## Operation

The submission endpoints (`add-chain`, `add-pre-chain`, and `get-roots`) are proxied to the log's submission endpoint without translation.

`get-entries` is converted to a single data tile fetch, and the response is translated to RFC 6962 syntax.  To build the response, issuer certificates are retrieved from the log as needed and cached.

`get-sth` returns the latest checkpoint retrieved from the log, translated to an RFC 6962 STH.

`get-sth-consistency` and `get-entry-and-proof` fetch the necessary tiles from the log to build a proof.

`get-proof-by-hash` is the most complicated endpoint to implement, since it requires determining the position of the leaf specified by the client.  Sunglasses continuously downloads leaf tiles from the log to build an index from leaf hash to leaf position.  `get-proof-by-hash` looks up the hash in the index, and then fetches the necessary tiles from the log to build a proof.

The leaf index, issuer cache, and latest STH are stored in a BoltDB database.

Note that `get-sth` only returns trees which have been fully indexed, and `get-entries` only returns entries within the tree returned by `get-sth`.  Consequentially, standing up a proxy for a large log takes a long time because all existing leaves have to be downloaded and indexed before the proxy is usable.  Once all leaves have been indexed, Sunglasses should have no problem keeping up with the growth of the log.

## Public Instances

These are for testing purposes only and should not be used in production.

* Itko 2025: `https://itko-2025.sunglasses.sslmate.net/`

## Installation

```
go install src.agwa.name/sunglasses@latest
```

## Command Line Arguments

### `-db PATH` (Mandatory)

Path to database file, which will be created if necessary.

### `-id BASE64` (Mandatory)

Log ID, in base64.

### `-listen SOCKET`

Listen on the given address, provided in [go-listener syntax](https://pkg.go.dev/src.agwa.name/go-listener#readme-listener-syntax).  You can specify the `-listen` flag multiple times to listen on multiple addresses.

### `-monitoring URL` (Mandatory)

URL prefix of the log's monitoring endpoint.

### `-submission URL` (Mandatory)

URL prefix of the log's submission endpoint.

### `-no-leaf-index`

Disable leaf indexing.  This considerably reduces the size of the database and allows you to stand up a proxy without waiting for the log to be indexed, but it means that the `get-proof-by-hash` endpoint won't work.

### `-unsafe-nofsync`

Dangerously disable fsync when writing to the database.  This is useful for speeding up the initial indexing, but if your system shuts down uncleanly you may experience database corruption, requiring you to reindex the log from scratch.  You should not use this flag once initial indexing is complete and the proxy is running in production.

## Example Usage

The following command will launch an RFC 6962-compatible log at `https://itko-2025.sunglasses.example.com` which proxies requests to the Itko 2025 log.

```
sunglasses -id o6U+hSdTxD0uzfNlqqAKPwLqLggaBqzejSxyrPxX18E= -db /srv/sunglasses/itko-2025.db -listen tls:itko-2025.sunglasses.example.com:tcp:443 -monitoring https://ct2025.itko.dev -submission https://ct2025.itko.dev
```
