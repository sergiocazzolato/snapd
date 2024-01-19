#!/bin/sh

set -e

echo Obtaining dependencies
export http_proxy="$HTTP_PROXY"
export HTTP_PROXY="$HTTP_PROXY"
export https_proxy="$HTTPS_PROXY"
export HTTPS_PROXY="$HTTPS_PROXY"
go mod vendor

echo Obtaining c-dependencies
(cd c-vendor && ./vendor.sh)

# TODO: import script that ensures that the "go.mod" vendor dir is tidy
# https://github.com/edgexfoundry/edgex-go/commit/2c7e513168ecd884ba7252d8253b100953d1695c
