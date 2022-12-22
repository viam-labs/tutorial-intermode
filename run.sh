#!/bin/sh
cd `dirname $0`

go build ./intermode-model
exec ./simplemodule $@
