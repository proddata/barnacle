Name:           barnacle
Version:        0.2.2
Release:        1%{?dist}
%global debug_package %{nil}
Summary:        HTTP and WebSocket proxy for PostgreSQL
# Barnacle is Apache-2.0; the statically linked Go standard library and modules
# also include MIT, BSD-3-Clause and ISC licensed code (see THIRD-PARTY-NOTICES.md).
License:        Apache-2.0 AND MIT AND BSD-3-Clause AND ISC
Source0:        %{name}-%{version}.tar.gz
Source1:        %{name}-%{version}-vendor.tar.gz
# The Go version is enforced by go.mod: GOTOOLCHAIN=local makes the build fail
# if the Go on PATH is older than it requires. Older Fedora releases need the
# upstream toolchain first on PATH.
BuildRequires:  golang
BuildRequires:  systemd-rpm-macros

%description
Barnacle carries PostgreSQL wire traffic over WebSocket and serves a
Neon-compatible subset of SQL over HTTP for one configured PostgreSQL server.

%prep
%setup -q -a 1

%build
export CGO_ENABLED=0
export GOTOOLCHAIN=local
export GOCACHE="%{_builddir}/barnacle-go-cache"
go build -mod=vendor -trimpath -buildmode=pie -ldflags='-s -w' -o barnacle .

%check
export GOTOOLCHAIN=local
export GOCACHE="%{_builddir}/barnacle-go-cache"
go test -mod=vendor ./...

%install
install -Dpm 0755 barnacle %{buildroot}%{_bindir}/barnacle
install -Dpm 0644 packaging/rpm/barnacle.service %{buildroot}%{_unitdir}/barnacle.service
install -Dpm 0644 packaging/rpm/barnacle.sysconfig %{buildroot}%{_sysconfdir}/sysconfig/barnacle
install -Dpm 0644 packaging/rpm/barnacle-key-access.conf %{buildroot}%{_datadir}/%{name}/barnacle-key-access.conf
install -Dpm 0644 LICENSE %{buildroot}%{_licensedir}/%{name}/LICENSE
install -Dpm 0644 THIRD-PARTY-NOTICES.md %{buildroot}%{_licensedir}/%{name}/THIRD-PARTY-NOTICES.md
for license in THIRD-PARTY-LICENSES/*; do
    install -Dpm 0644 "$license" "%{buildroot}%{_licensedir}/%{name}/$license"
done

%post
%systemd_post barnacle.service

%preun
%systemd_preun barnacle.service

%postun
%systemd_postun_with_restart barnacle.service

%files
%doc README.md
%license %{_licensedir}/%{name}
%{_bindir}/barnacle
%{_unitdir}/barnacle.service
%{_datadir}/%{name}/barnacle-key-access.conf
%config(noreplace) %{_sysconfdir}/sysconfig/barnacle

%changelog
* Tue Oct 13 2026 Barnacle contributors - 0.2.2-1
- Bound incoming main and separate metrics TCP connections, including WebSockets
- Account for result metadata in the shared HTTP memory budget
- Set Debian's Go memory limit to match the RPM memory profile
- Require full CI validation before publishing release artifacts

* Mon Oct 12 2026 Barnacle contributors - 0.2.1-1
- Republish 0.2.0 as 0.2.1: the 0.2.0 release was withdrawn and its tag cannot be reused
- No functional changes from 0.2.0

* Mon Oct 12 2026 Barnacle contributors - 0.2.0-1
- Add -check-config and -strict to validate the BARNACLE_* environment
- Send a WebSocket close frame when PostgreSQL ends a session
- Build the package as a position-independent executable
- Add maxRows to cap rows returned by buffered /sql queries
- Document configuring the packaged service with a drop-in

* Thu Oct 01 2026 Barnacle contributors - 0.1.0-1
- Initial RPM package
