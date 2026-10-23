# btproxy RPM spec.
#
# Build from a git checkout (adjust the version):
#
#   git archive --prefix=btproxy-0.1.0/ -o ~/rpmbuild/SOURCES/btproxy-0.1.0.tar.gz HEAD
#   rpmbuild -bb btproxy.spec
#
# The build downloads Go modules (godbus, testify); for fully offline
# builds, run `go mod vendor` and add -mod=vendor to the go commands below.

%global goflags CGO_ENABLED=0
# Go build produces no separate debug sources; skip debug subpackages.
%global debug_package %{nil}

Name:           btproxy
Version:        0.1.0
# Plain release builds use "1". Build with `make rpm` (or pass
# --define 'githash <sha>') to get a snapshot release "0.git<sha>", which
# sorts below "1" so a formal release always upgrades a git snapshot.
Release:        %{?githash:0.git%{githash}}%{!?githash:1}%{?dist}
Summary:        ESPHome-native Bluetooth proxy for Home Assistant

License:        MIT
URL:            https://github.com/ankon/btproxy
Source0:        %{url}/archive/v%{version}/%{name}-%{version}.tar.gz

BuildRequires:  golang >= 1.26
Requires:       bluez
Recommends:     avahi
%{?systemd_requires}

%description
btproxy makes a Linux host act as a passive ESPHome Bluetooth proxy for
Home Assistant: it scans for BLE advertisements through the BlueZ D-Bus
API (the system bluetoothd keeps running normally) and serves them to
Home Assistant over the ESPHome native API on TCP 6053.

%prep
%setup -q

%build
# api_gen.go is generated from api.pidl via the tinypbc generator.
%{goflags} go generate ./...
%{goflags} go build -trimpath -ldflags "-s -w -X main.version=%{version}-%{release}" -o bin/btproxy ./cmd/btproxy

%check
%{goflags} go test ./...

%install
install -Dpm0755 bin/btproxy %{buildroot}%{_bindir}/btproxy
# The unit in the source tree points at /usr/local/bin (the manual
# `make install` flow); packaged binaries live in /usr/bin per FHS, so
# rewrite the path when installing the unit into the package.
mkdir -p %{buildroot}%{_unitdir}
sed -e 's|/usr/local/bin/btproxy|%{_bindir}/btproxy|' \
    systemd/btproxy.service > %{buildroot}%{_unitdir}/btproxy.service
# Into firewalld's system services directory: the service definition is
# then known to firewalld without copying to /etc.
install -Dpm0644 systemd/btproxy-firewalld.xml %{buildroot}%{_prefix}/lib/firewalld/services/btproxy.xml

%post
%systemd_post btproxy.service

%preun
%systemd_preun btproxy.service

%postun
%systemd_postun btproxy.service

%files
%license LICENSE
%doc README.md
%{_bindir}/btproxy
%{_unitdir}/btproxy.service
%{_prefix}/lib/firewalld/services/btproxy.xml

%changelog
* Fri Oct 23 2026 Andreas Kohn <andreas.kohn@gmail.com> - 0.1.0-1
- Initial packaging.
