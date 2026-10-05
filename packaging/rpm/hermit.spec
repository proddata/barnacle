Name:           hermit
Version:        0.1.0
Release:        1%{?dist}
%global debug_package %{nil}
Summary:        HTTP and WebSocket proxy for PostgreSQL
# Hermit is Apache-2.0; the statically linked Go standard library and modules
# also include MIT and BSD-3-Clause licensed code (see THIRD-PARTY-NOTICES.md).
License:        Apache-2.0 AND MIT AND BSD-3-Clause
Source0:        %{name}-%{version}.tar.gz
Source1:        %{name}-%{version}-vendor.tar.gz
BuildRequires:  golang >= 1.25.14
BuildRequires:  systemd-rpm-macros

%description
Hermit carries PostgreSQL wire traffic over WebSocket and serves a
Neon-compatible subset of SQL over HTTP for one configured PostgreSQL server.

%prep
%setup -q -a 1

%build
export CGO_ENABLED=0
export GOTOOLCHAIN=local
export GOCACHE="%{_builddir}/hermit-go-cache"
go build -mod=vendor -trimpath -ldflags='-s -w' -o hermit .

%check
export GOTOOLCHAIN=local
export GOCACHE="%{_builddir}/hermit-go-cache"
go test -mod=vendor ./...

%install
install -Dpm 0755 hermit %{buildroot}%{_bindir}/hermit
install -Dpm 0644 packaging/rpm/hermit.service %{buildroot}%{_unitdir}/hermit.service
install -Dpm 0644 packaging/rpm/hermit.sysconfig %{buildroot}%{_sysconfdir}/sysconfig/hermit
install -Dpm 0644 packaging/rpm/hermit-key-access.conf %{buildroot}%{_datadir}/%{name}/hermit-key-access.conf
install -Dpm 0644 LICENSE %{buildroot}%{_licensedir}/%{name}/LICENSE
install -Dpm 0644 THIRD-PARTY-NOTICES.md %{buildroot}%{_licensedir}/%{name}/THIRD-PARTY-NOTICES.md
for license in THIRD-PARTY-LICENSES/*; do
    install -Dpm 0644 "$license" "%{buildroot}%{_licensedir}/%{name}/$license"
done

%post
%systemd_post hermit.service

%preun
%systemd_preun hermit.service

%postun
%systemd_postun_with_restart hermit.service

%files
%doc README.md
%license %{_licensedir}/%{name}
%{_bindir}/hermit
%{_unitdir}/hermit.service
%{_datadir}/%{name}/hermit-key-access.conf
%config(noreplace) %{_sysconfdir}/sysconfig/hermit

%changelog
* Thu Oct 01 2026 Hermit contributors - 0.1.0-1
- Initial RPM package
