#!/bin/bash

# This script builds all images locally except the base and release images,
# which are handled by hack/build-base-images.sh.

# NOTE:  you only need to run this script if your code changes are part of
# any images OpenShift runs internally such as origin-sti-builder, origin-docker-builder,
# origin-deployer, etc.

set -o errexit
set -o nounset
set -o pipefail

STARTTIME=$(date +%s)
OS_ROOT=$(dirname "${BASH_SOURCE}")/..
source "${OS_ROOT}/hack/common.sh"
source "${OS_ROOT}/hack/util.sh"
source "${OS_ROOT}/contrib/node/install-sdn.sh"
os::log::install_errexit

# Go to the top of the tree.
cd "${OS_ROOT}"

if [[ "${OS_RELEASE:-}" == "n" ]]; then
  # Use local binaries
  imagedir="${OS_OUTPUT_BINPATH}/linux/amd64"
  # identical to build-cross.sh
  os::build::os_version_vars
  OS_RELEASE_COMMIT="${OS_GIT_SHORT_VERSION}"
  OS_BUILD_PLATFORMS=("${OS_IMAGE_COMPILE_PLATFORMS[@]-}")

  echo "Building images from source ${OS_RELEASE_COMMIT}:"
	echo
  os::build::build_static_binaries "${OS_IMAGE_COMPILE_TARGETS[@]-}" "${OS_SCRATCH_IMAGE_COMPILE_TARGETS[@]-}"
	os::build::place_bins "${OS_IMAGE_COMPILE_BINARIES[@]}"
  echo
else
# Get the latest Linux release
  if [[ ! -d _output/local/releases ]]; then
    echo "No release has been built. Run hack/build-release.sh"
    exit 1
  fi

  # Extract the release achives to a staging area.
  os::build::detect_local_release_tars "linux-64bit"

  echo "Building images from release tars for commit ${OS_RELEASE_COMMIT}:"
  echo " primary: $(basename ${OS_PRIMARY_RELEASE_TAR})"
  echo " image:   $(basename ${OS_IMAGE_RELEASE_TAR})"

  imagedir="${OS_OUTPUT}/images"
  rm -rf ${imagedir}
  mkdir -p ${imagedir}
  tar xzpf "${OS_PRIMARY_RELEASE_TAR}" --strip-components=1 -C "${imagedir}"
  tar xzpf "${OS_IMAGE_RELEASE_TAR}" --strip-components=1 -C "${imagedir}"
fi

# Copy primary binaries to the appropriate locations.
ln -f "${imagedir}/openshift" images/origin/bin
ln -f "${imagedir}/openshift" images/router/haproxy/bin
ln -f "${imagedir}/openshift" images/ipfailover/keepalived/bin

# Copy image binaries to the appropriate locations.
ln -f "${imagedir}/pod"             images/pod/bin
ln -f "${imagedir}/hello-openshift" examples/hello-openshift/bin
ln -f "${imagedir}/deployment"      examples/deployment/bin
ln -f "${imagedir}/gitserver"       examples/gitserver/bin
ln -f "${imagedir}/dockerregistry"  images/dockerregistry/bin
ln -f "${imagedir}/recycle"         images/recycler/bin

# Copy SDN scripts into images/node
os::provision::install-sdn "${OS_ROOT}" "${OS_ROOT}/images/node"
mkdir -p images/node/conf/
cp -pf "${OS_ROOT}/contrib/systemd/openshift-sdn-ovs.conf" images/node/conf/

# builds an image and tags it two ways - with latest, and with the release tag
function image {
  STARTTIME=$(date +%s)
  echo "--- $1 ---"
  docker build -t $1:latest $2
  docker tag -f $1:latest $1:${OS_RELEASE_COMMIT}
  git clean -fdx $2
  ENDTIME=$(date +%s); echo "--- $1 took $(($ENDTIME - $STARTTIME)) seconds ---"
  echo
  echo
}

# images that depend on scratch / centos
image openshift/origin-pod                   images/pod
image openshift/openvswitch                  images/openvswitch
# images that depend on openshift/origin-base
image openshift/origin                       images/origin
image openshift/origin-haproxy-router        images/router/haproxy
image openshift/origin-keepalived-ipfailover images/ipfailover/keepalived
image openshift/origin-docker-registry       images/dockerregistry
# images that depend on openshift/origin
image openshift/origin-deployer              images/deployer
image openshift/origin-recycler              images/recycler
image openshift/origin-docker-builder        images/builder/docker/docker-builder
image openshift/origin-gitserver             examples/gitserver
image openshift/origin-sti-builder           images/builder/docker/sti-builder
image openshift/origin-f5-router             images/router/f5
image openshift/node                         images/node
# unpublished images
image openshift/origin-custom-docker-builder images/builder/docker/custom-docker-builder

# extra images (not part of infrastructure)
image openshift/hello-openshift              examples/hello-openshift
docker build --no-cache -t openshift/deployment-example:v1 examples/deployment
docker build --no-cache -t openshift/deployment-example:v2 -f examples/deployment/Dockerfile.v2 examples/deployment

echo
echo
echo "++ Active images"

docker images | grep openshift/ | grep ${OS_RELEASE_COMMIT} | sort
echo

ret=$?; ENDTIME=$(date +%s); echo "$0 took $(($ENDTIME - $STARTTIME)) seconds"; exit "$ret"
