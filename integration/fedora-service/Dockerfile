FROM fedora:44
RUN dnf install -y systemd curl openssl shadow-utils util-linux-user && dnf clean all
CMD ["/sbin/init"]
