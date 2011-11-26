export GOPATH=$(readlink -m `dirname $0`)

find $GOPATH/src -name '*.go' | xargs gofmt -w
goinstall mksysimage.googlecode.com/hg/mksysimage
