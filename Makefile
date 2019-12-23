# My preferred way to quickly test distri in a somewhat real environment is to
# use qemu (with KVM acceleration).
#
# To build an image in DISKIMG (default /tmp/root.img) which is ready to be used
# via qemu-serial¹, use e.g.:
#   % make image serial=1
#   % make qemu-serial
#
# If you want a graphical output instead, use e.g.:
#   % make image
#   % make qemu-graphic
#
# To test an encrypted root file system, substitute the image target with the
# cryptimage target.
#
# ① Unfortunately, the linux console can only print to one device.

DISKIMG=/tmp/distri-disk.img
GCEDISKIMG=/tmp/distri-gcs.tar.gz
DOCSDIR=/tmp/distri-docs

QEMU=qemu-system-x86_64 \
	--bios /usr/share/ovmf/x64/OVMF_CODE.fd \
	-device e1000,netdev=net0 \
	-netdev user,id=net0,hostfwd=tcp::5555-:22 \
	-device virtio-rng-pci \
	-smp 8 \
	-machine accel=kvm \
	-m 4096 \
	-drive if=none,id=hd,file=${DISKIMG},format=raw \
	-device virtio-scsi-pci,id=scsi \
	-device scsi-hd,drive=hd

PACKFLAGS=

# for when you want to see non-kernel console output (e.g. systemd), useful with qemu
ifdef serial
PACKFLAGS+= -serialonly
endif

ifdef authorized_keys
PACKFLAGS+= -authorized_keys=${authorized_keys}
endif

ifdef branch
PACKFLAGS+= -branch=${branch}
endif

IMAGE=distri pack \
	-diskimg=${DISKIMG} \
	-base=base-x11 ${PACKFLAGS}

GCEIMAGE=distri pack \
	-gcsdiskimg=${GCEDISKIMG} \
	-base=base-cloud ${PACKFLAGS}

DOCKERTAR=distri pack -docker ${PACKFLAGS}

.PHONY: install

all: PACKFLAGS+= -serialonly
all: clear base image qemu-serial

install:
# TODO: inherit CAP_SETFCAP
	CGO_ENABLED=0 go install ./cmd/... && sudo setcap 'CAP_SYS_ADMIN,CAP_DAC_OVERRIDE=ep CAP_SETFCAP=eip' ~/go/bin/distri

test: install
	DISTRIROOT=$$PWD go test -v ./cmd/... ./integration/...

distri1: install
	cd pkgs/distri1 && distri

base: distri1
	cd pkgs/base && distri
	cd pkgs/base-full && distri
	cd pkgs/base-x11 && distri

clear:
	-rm -R build/base*
	-rm -R build/distri1
	-rm -R build/distri/pkg/base*
	-rm -R build/distri/pkg/distri1*

image: install
	DISTRIROOT=$$PWD ${IMAGE}

cryptimage:
	DISTRIROOT=$$PWD ${IMAGE} -encrypt

gceimage:
	DISTRIROOT=$$PWD ${GCEIMAGE}

dockertar:
	@DISTRIROOT=$$PWD ${DOCKERTAR}

qemu-serial:
	${QEMU} -nographic

qemu-graphic:
	${QEMU}

.PHONY: docs

docs: docs/building.asciidoc docs/package-format.asciidoc docs/index.asciidoc docs/rosetta-stone.asciidoc
	mkdir -p ${DOCSDIR}
	asciidoctor --destination-dir ${DOCSDIR} $^
