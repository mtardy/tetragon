#!/bin/bash

set -e
set -o pipefail
shopt -s expand_aliases

# make this script executable from anywhere
SCRIPT_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
cd $SCRIPT_DIR

alias helm='docker run --rm -v $(pwd):/apps alpine/helm:3.3.4'
alias kubeval='docker run --rm -i garethr/kubeval:0.15.0'
helm dependency update .
helm lint . --with-subcharts
helm template tetragon . | kubeval --strict --additional-schema-locations https://raw.githubusercontent.com/joshuaspence/kubernetes-json-schema/master

# Update README.md.
docker run --rm -v "$(pwd):/helm-docs" -u "$(id -u)" jnorwood/helm-docs:v1.2.1
