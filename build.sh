export GOPATH=$(readlink -m `dirname $0`)
mkdir -p src/mksysimage.googlecode.com/hg
ln -sf $GOPATH/mksysimage src/mksysimage.googlecode.com/hg/mksysimage
find $GOPATH/src -name '*.go' | xargs gofmt -w
goinstall -nuke mksysimage.googlecode.com/hg/mksysimage
