# syntax=docker/dockerfile:1
FROM alpine:3.21

RUN apk add --no-cache ca-certificates openssl openssh-keygen

COPY generate.sh /usr/local/bin/generate.sh
RUN chmod 0755 /usr/local/bin/generate.sh

ENTRYPOINT ["/usr/local/bin/generate.sh"]
