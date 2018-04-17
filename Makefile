all: duopull

# Package lambda function in zip file
package:
	docker run -i --rm -v `pwd`:/go/src/github.com/mozilla-services/duopull-lambda \
		golang:1.7 \
		/bin/bash -c 'cd /go/src/github.com/mozilla-services/duopull-lambda && make lambda'

# Development target, upload package to s3
packageupload:
	@if [[ -z "$(DUOPULL_S3_BUCKET)" ]]; then \
		echo "set DUOPULL_S3_BUCKET in environment"; \
		exit 1; \
	fi
	aws s3 cp duopull.zip s3://$(DUOPULL_S3_BUCKET)/duopull.zip

lambda: clean dep duopull
	apt-get update
	apt-get install -y zip
	zip duopull.zip duopull

dep:
	go get ./...

duopull: duopull.go
	go build -ldflags="-s -w" duopull.go

debugduo: duopull
	env DEBUGDUO=1 ./duopull

clean:
	rm -f duopull duopull.zip

.PHONY: clean dep debugduo lambda package packageupload
