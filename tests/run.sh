#!/bin/sh

cd `dirname $0` && go build -o test-run \
&& cd .. && go build && ./empi -c ./tests/test.config.json

