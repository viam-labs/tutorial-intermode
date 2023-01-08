#!/bin/sh

go build ./
exec ./intermode-base $@
