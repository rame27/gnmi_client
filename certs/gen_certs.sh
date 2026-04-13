#!/bin/bash -x

rm -rf client.crt client.csr client.key rootCA.crt rootCA.key server.crt server.csr server.key

openssl genrsa -out rootCA.key 4096
openssl req -x509 -new -nodes -key rootCA.key -sha256 -days 3650 \
  -out rootCA.crt \
  -subj "/C=IN/ST=KA/L=BLR/O=Test/OU=Net/CN=admin"

openssl genrsa -out server.key 2048
openssl req -new -key server.key -out server.csr -config ./server.cnf
openssl x509 -req -in server.csr \
  -CA rootCA.crt -CAkey rootCA.key -CAcreateserial \
  -out server.crt -days 365 -sha256 \
  -extensions req_ext -extfile server.cnf

openssl genrsa -out client.key 2048
openssl req -new -key client.key -out client.csr -config ./client.cnf
openssl x509 -req -in client.csr \
  -CA rootCA.crt -CAkey rootCA.key -CAcreateserial \
  -out client.crt -days 365 -sha256 \
  -extensions req_ext -extfile client.cnf


