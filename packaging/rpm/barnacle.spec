Name:           barnacle
Version:        0.1.0
Release:        1%{?dist}
%global debug_package %{nil}
Summary:        HTTP and WebSocket proxy for PostgreSQL
# Barnacle is Apache-2.0; the statically linked Go standard library and modules
# also include MIT and BSD-3-Clause licensed code (see THIRD-PARTY-NOTICES.md).
License:        Apache-2.0 AND MIT AND BSD-3-Clause
Source0:        %{name}-%{version}.tar.gz
Source1:        %{name}-%{version}-vendor.tar.gz
BuildRequires:  golang >= 1.25.14
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
go build -mod=vendor -trimpath -ldflags='-s -w' -o barnacle .

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
* Thu Oct 01 2026 Barnacle contributors - 0.1.0-1
- Initial RPM package
