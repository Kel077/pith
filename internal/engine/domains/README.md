# Domain observation

TCP reads at most 64 KiB under one short deadline and returns every consumed
byte for forwarding. It recognizes HTTP/1 Host and TLS ClientHello SNI, including
handshakes split across TLS records. No TLS interception or application-data
decryption is performed. ECH can hide the real name; encrypted DNS and names not
present on the wire cannot always be recovered.

QUIC v1/v2 client Initials use their public Initial keys, independently
implemented from [RFC 9001 sections 5.2–5.4](https://www.rfc-editor.org/rfc/rfc9001.html#section-5.2)
and [RFC 9369 section 3](https://www.rfc-editor.org/rfc/rfc9369.html#section-3).
CRYPTO frames within one Initial can be fragmented or out of order. Cross-datagram
fragmentation, retry exchanges and unknown versions are best effort. Authentication
must pass before reading the ClientHello. The original datagram is never changed.

DNS observations pair requests and responses on the same session, verify IDs and
questions, and accept only answer addresses reachable through the question's
CNAME chain. UDP and framed TCP are supported. TTLs are capped at one hour; the
cache and pending requests are bounded. Several live domains sharing an address
produce no guessed name. Sniffed SNI/Host takes precedence over this cache.

Tests generate ClientHellos locally with Go TLS, check public RFC key vectors,
exercise timeout, malformed/fragmented input, request correlation and CNAME TTLs,
and fuzz every parser. They do not capture user traffic or alter networking.
