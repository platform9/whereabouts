src_dir=$(shell pwd)
go_cmd=go
repo=platform9
version ?= $(shell git describe --tags HEAD |  sed 's/-.*//')

#registry_url ?= 514845858982.dkr.ecr.us-west-1.amazonaws.com
registry_url ?= docker.io

image_name = ${registry_url}/platform9/whereabouts
image_tag = $(version)-pmk-$(TEAMCITY_BUILD_ID)

SRC_ROOT=$(abspath $(dir $(lastword $(MAKEFILE_LIST)))/)
BUILD_ROOT = $(SRC_ROOT)/build
TAG=$(image_name):${image_tag}


image:
	@echo $(TAG)
	docker build -t $(TAG) .

push: image
	docker push $(TAG) \
	&& docker rmi $(TAG)

scan: 
	mkdir -p $(BUILD_DIR)/profile-agent
	docker run -v $(BUILD_ROOT)/whereabouts:/out -v /var/run/docker.sock:/var/run/docker.sock  aquasec/trivy image -s CRITICAL,HIGH -f json  --vuln-type library -o /out/library_vulnerabilities.json --exit-code 22 ${TAG}
	docker run -v $(BUILD_ROOT)/whereabouts:/out -v /var/run/docker.sock:/var/run/docker.sock  aquasec/trivy image -s CRITICAL,HIGH -f json  --vuln-type os -o /out/os_vulnerabilities.json --exit-code 22 ${TAG}
