export GOPATH=$(readlink -m `dirname $0`)
mkdir -p src/mksysimage.googlecode.com/hg/mksysimage
ln -f $GOPATH/main.go src/mksysimage.googlecode.com/hg/mksysimage/main.go
find $GOPATH/src -name '*.go' | xargs gofmt -w
goinstall -nuke mksysimage.googlecode.com/hg/mksysimage
