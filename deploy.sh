#!/bin/bash


n=`date '+prom-%Y-%m-%d--%H-%M.tar.gz'`
echo $n


go install ./...

cd $GOPATH

rm -rf _bin
mkdir _bin

git -C $TRAVIS_BUILD_DIR log -10 > _bin/_version

echo "$n" > _bin/_pack
echo "$n"

md5sum bin/prometheus > _bin/_md5sum

cp bin/prometheus _bin/

echo -n 'tar ...'
tar czf $n _bin
echo ' [done]'

ls -lh _bin/
ls -lh $n

echo -n 'upload ...'
curl -T $n -e 'upload-57d6ce1e-e41e-4bc1-afe4-f6413159cd74' -H 'Host: om-deploy.sh1a.qingstor.com' 'http://hk.opsmind.com/'
echo ' [done]'

rm -rf _bin
