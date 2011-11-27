#!/bin/bash

eval `go-switch 386`
./build.sh
mv -f bin/mksysimage builds/mksysimage.386

eval `go-switch amd64`
./build.sh
mv -f bin/mksysimage builds/mksysimage.amd64
