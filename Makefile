src_dir=$(shell pwd)
go_cmd=go
repo=platform9
version=$(shell git describe --tags HEAD)

registry_url ?= 514845858982.dkr.ecr.us-west-1.amazonaws.com
#registry_url ?= docker.io

image_name = ${registry_url}/platform9/whereabouts
image_tag = $(version)-pmk-$(TEAMCITY_BUILD_ID)

TAG=$(image_name):${image_tag}


image:
	@echo $(TAG)
	docker build -t $(TAG) .

push: image
	docker push $(TAG) \
	&& docker rmi $(TAG)
	(docker push $(TAG}  || \
		(aws ecr get-login --region=us-west-1 --no-include-email | sh && \
		docker push $(TAG))) && \
		docker rmi $(TAG)


