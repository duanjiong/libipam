# Copyright 2020 The KubeSphere Authors. All rights reserved.
# Use of this source code is governed by an Apache license
# that can be found in the LICENSE file.
FROM centos:7

#RUN apk add --no-cache ca-certificates  bash
RUN mkdir -p /opt/cni/bin
RUN mkdir -p /etc/cni/net.d/

COPY  ipam /opt/cni/bin
COPY  build/cnitool /usr/local/bin
COPY  build/ipam.conf /etc/cni/net.d/

EXPOSE 9090
CMD ["sh"]