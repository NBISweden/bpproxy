# bpproxy

## Introduction

This is a proxy service as a workaround to figure out how the[sensitive data
archive](https://github.com/neicnordic/sensitive-data-archive) should behave.
While this code should be usable, the long term goal is that
it shouldn't be needed. Possibly it will live on as a means to provide local
caching to improve perceived performance when using datasets.

This work has been done in the context of the
[bigpicture project](https://bigpicture.eu/) but should
benefit other uses of the sensitive data archive stack, e.g. Federated EGA or
GDI.

## Background

While the SDA stack offers an s3-like interface, it wasn't part of the base
design and hasn't been an important part of the archive and has thus seen
limited exposure and limited real life testing.

This causes problems with a s3-like interface where some things simply don't
apply and different clients behave differently.

The goal of this project is to figure out what changes would be needed to offer
an interface that works with a wider variety of softwares.

## Usage

The expected use of this service is to run as an intermediate to an instance
of the sensitive data archive and a local consumer, e.g. a FUSE adapter for S3
or some other S3 consumer.

Since most instances of the sensitive data archive will be unwilling to data
that is not encrypted with crypt4gh and most s3 client makes it difficult to
customize the request as necessary, this tooling supports specifying a default
public key that is used for data encryption if the request doesn't already
specify a key.

Example usage would be

```bash
bpproxy https://download.bp.nbis.se/s3-encrypted &

export AWS_SESSION_TOKEN=YOURACCESTOKEN
export AWS_SECRET_ACCESS_KEY=something
export AWS_ACCESS_KEY_ID=else

geesefs --endpoint http://127.0.0.1:8090/  datasetbane  /mount/point
```

## Configuration

Configuration can be done either through environment variables or with a YAML
file.

The currently supported configuration options are:

* `baseURL` (environment variable `BPPROXY_BASEURL`), this is endpoint URL for
the service the proxy will contact. It will probably be called download or
similar and a simple `GET` of it with correct credentials should produce a
response with `<ListAllMyBucketsResult>` in it.
* `server.listenAddress` (environment variable `BPPROXY_SERVER_LISTENADDRESS`),
this is where the proxy service will listen for connections, defaults to
`127.0.0.1:8090` meaning port 8090 on localhost.
* `server.keyFile` (environment variable `BPPROXY_SERVER_KEYFILE`), if set
this is the path to a keyfile that will be used to serve TLS. Both this and
`server.chainFile` need to be set for the proxy to accept incoming TLS
connections. This is unset by default (meaning the serve will not
accept incoming TLS connections).
* `server.chainFile` (environment variable `BPPROXY_SERVER_CHAINFILE`), if set
this is the path to a X509 certificate chain file that will be used to serve
TLS. Both this and `server.keyFile` need to be set for the proxy to accept
incoming TLS connections. This is unset by default (meaning the serve will not
accept incoming TLS connections).
* `defaultKey` (environment variable `BPPROXY_DEFAULTKEY`), if set, determines
what crypt4gh public key to use as default if the actual request being relayed
does not specify any. This can be either a base64 encoding of an actual public
key in PEM format or alternatively the path of a file containing the public key
in PEM format.

## FAQ
### Where do I supply credentials

The proxy tools keeps/manages no credentials by itself, all such things come
from the s3 consumer.

### What clients are used for testing

The goal is to increase the variety of clients supported. Right now
[geesesfs](https://github.com/yandex-cloud/geesefs) works fairly well whereas
[s3fs](https://github.com/s3fs-fuse/s3fs-fuse) and
[mountpoint-s3](https://github.com/awslabs/mountpoint-s3) do not appear to be
useful yet.

If you know of some other (easily accessible) client where better support would
benefit the world, please let us know.

### Does the client only proxy and transform the queries/responses

No, it also has a naive cache implemented for metadata intended to help with
list operations.

Since the expected use case for this tool is the sensitive data archive where
access is expected to be read only, it has no notion of things changing on the
backend.

The caching system rudimentary ties the metadata in the cache to the
authorization used to acquire it to prevent data leakage.

### Logs

The proxy is still to be considered a work in progress and is likely
to output copious amounts of logs that may not be useful if you have an issue.

Logs are not guaranteed to be cleaned from sensitive details (it's in fact more
than likely that they contain data that should not be public), so handle with
care.
