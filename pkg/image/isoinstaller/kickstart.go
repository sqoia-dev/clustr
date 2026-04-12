package isoinstaller

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// AutoInstallConfig holds the rendered automated-install configuration files
// for a given distro. Different distros need different files in different places.
type AutoInstallConfig struct {
	// Format identifies which installer automation format was generated.
	Format AutoInstallFormat

	// KickstartContent is the rendered kickstart / preseed / autoinstall file.
	// For RHEL-family this is the ks.cfg content.
	// For Ubuntu this is the user-data content (cloud-init autoinstall).
	// For Debian this is the preseed.cfg content.
	KickstartContent string

	// MetaDataContent is the cloud-init meta-data content (Ubuntu only).
	// Empty for non-Ubuntu distros.
	MetaDataContent string

	// ISOLabel is the volume label that causes Anaconda / the installer to
	// auto-discover the kickstart drive. OEMDRV for RHEL, CIDATA for Ubuntu.
	ISOLabel string
}

// templateData holds the variables injected into each install config template.
type templateData struct {
	// RootPasswordHash is a SHA-512 crypt(3) hash of a fixed per-build password.
	RootPasswordHash string
	// DiskSizeGB is the target disk size, used in preseed size hints.
	DiskSizeGB int
}

const defaultRootPasswordHash = "$6$rounds=4096$clonr$oJJBrlGPtKS6kxQe7yLm.lXX/XKNEDXkJxhXbXONnR5Rb2FIWKijYcpg/0E1n3W6B9Ik8n3Zd7gH8kO35i3o1"

// GenerateAutoInstallConfig produces the automated-install configuration for
// the given distro and build options. Returns an AutoInstallConfig with the
// rendered file content(s) ready to be written to a seed ISO.
func GenerateAutoInstallConfig(distro Distro, opts BuildOptions, customKickstart string) (*AutoInstallConfig, error) {
	data := templateData{
		RootPasswordHash: defaultRootPasswordHash,
		DiskSizeGB:       opts.DiskSizeGB,
	}

	switch distro.Format() {
	case FormatKickstart:
		return generateKickstart(distro, data, opts, customKickstart)
	case FormatAutoInstall:
		return generateUbuntuAutoInstall(data, opts)
	case FormatPreseed:
		return generateDebianPreseed(data)
	case FormatAutoYaST:
		return generateAutoYaST(data)
	case FormatAnswers:
		return generateAlpineAnswers(data)
	default:
		return generateKickstart(distro, data, opts, customKickstart)
	}
}

// ── RHEL family (Rocky / Alma / CentOS / RHEL) ───────────────────────────────

// ksTemplateData is injected into the RHEL kickstart template.
type ksTemplateData struct {
	templateData
	Distro         string
	RoleIDs        []string
	Packages       []string
	Services       []string
	InstallUpdates bool
	NeedsNVIDIA    bool
	NeedsLustre    bool
	NeedsBeeGFS    bool
}

// joinStrings is the template func for joining slices — kept here so the
// "strings" import is actually referenced and the compiler is happy.
func joinStrings(sep string, elems []string) string { return strings.Join(elems, sep) }

var kickstartTemplate = template.Must(template.New("ks").Funcs(template.FuncMap{
	"join": func(elems []string, sep string) string { return strings.Join(elems, sep) },
}).Parse(`# clonr auto-generated kickstart
# Distro: {{.Distro}}
# Roles:  {{join .RoleIDs ", "}}
# This kickstart produces a minimal, identity-scrubbed base image suitable
# for capture as a clonr BaseImage. It is NOT intended as a production kickstart.
cdrom
lang en_US.UTF-8
keyboard us
timezone UTC --utc
rootpw --iscrypted {{.RootPasswordHash}}
selinux --permissive
firewall --disabled
network --bootproto=dhcp --device=link --activate
skipx
firstboot --disabled

zerombr
clearpart --all --initlabel --disklabel=gpt
bootloader --location=mbr --driveorder=sda --append="console=ttyS0,115200"
part biosboot --fstype=biosboot   --size=1     --ondisk=sda --label=biosboot
part /boot    --fstype=xfs        --size=1024  --ondisk=sda --label=boot
part /        --fstype=xfs        --size=1     --grow        --ondisk=sda --label=root

%packages --ignoremissing
@^minimal-environment
openssh-server
{{- range .Packages}}
{{.}}
{{- end}}
%end

%post --log=/root/ks-post.log
# ── Base services ──────────────────────────────────────────────────────────
systemctl enable sshd
{{range .Services -}}
systemctl enable {{.}}
{{end -}}
{{- if .InstallUpdates}}
# ── Install OS updates ─────────────────────────────────────────────────────
dnf update -y
{{end -}}
{{- if .NeedsNVIDIA}}
# ── NVIDIA / CUDA (gpu-compute role) ──────────────────────────────────────
# Adds the NVIDIA CUDA repo for RHEL 9 / Rocky 9 / Alma 9.
dnf config-manager --add-repo https://developer.download.nvidia.com/compute/cuda/repos/rhel9/x86_64/cuda-rhel9.repo
dnf install -y kernel-devel-$(uname -r) kernel-headers-$(uname -r) || true
dnf install -y cuda-drivers cuda-toolkit nvidia-driver-NVML || true
systemctl enable nvidia-persistenced || true
{{end -}}
{{- if .NeedsLustre}}
# ── Lustre client (storage role) ──────────────────────────────────────────
dnf config-manager --add-repo https://downloads.whamcloud.com/public/lustre/latest-release/el9.4/client/
dnf install -y lustre-client --nogpgcheck || true
{{end -}}
{{- if .NeedsBeeGFS}}
# ── BeeGFS (storage role) ─────────────────────────────────────────────────
wget -q https://www.beegfs.io/release/beegfs_7.4/dists/beegfs-rhel9.repo -O /etc/yum.repos.d/beegfs-rhel9.repo || true
dnf install -y beegfs-storage beegfs-meta --nogpgcheck || true
{{end}}
# ── Strip node identity — regenerated on first boot by clonr finalize ──────
rm -f /etc/machine-id
touch /etc/machine-id
rm -f /etc/ssh/ssh_host_*
rm -f /etc/NetworkManager/system-connections/*
%end

reboot --eject
`))

func generateKickstart(distro Distro, data templateData, opts BuildOptions, customKickstart string) (*AutoInstallConfig, error) {
	if customKickstart != "" {
		return &AutoInstallConfig{
			Format:           FormatKickstart,
			KickstartContent: customKickstart,
			ISOLabel:         "OEMDRV",
		}, nil
	}

	packages, services := MergeRoles(opts.RoleIDs, distro)

	roleIDs := opts.RoleIDs
	if len(roleIDs) == 0 {
		roleIDs = []string{"(none — minimal install)"}
	}

	d := ksTemplateData{
		templateData:   data,
		Distro:         distro.FamilyName(),
		RoleIDs:        roleIDs,
		Packages:       packages,
		Services:       services,
		InstallUpdates: opts.InstallUpdates,
		NeedsNVIDIA:    hasRole(opts.RoleIDs, "gpu-compute"),
		NeedsLustre:    hasRole(opts.RoleIDs, "storage"),
		NeedsBeeGFS:    hasRole(opts.RoleIDs, "storage"),
	}

	var buf bytes.Buffer
	if err := kickstartTemplate.Execute(&buf, d); err != nil {
		return nil, fmt.Errorf("render kickstart: %w", err)
	}
	return &AutoInstallConfig{
		Format:           FormatKickstart,
		KickstartContent: buf.String(),
		ISOLabel:         "OEMDRV",
	}, nil
}

// ── Ubuntu (autoinstall / cloud-init) ────────────────────────────────────────

// ubuntuTemplateData extends templateData with role-specific fields for Ubuntu.
type ubuntuTemplateData struct {
	templateData
	Packages       []string
	Services       []string
	InstallUpdates bool
}

var ubuntuUserDataTemplate = template.Must(template.New("ubuntu-ud").Parse(`#cloud-config
autoinstall:
  version: 1
  identity:
    hostname: generic
    username: root
    password: "{{.RootPasswordHash}}"
  locale: en_US.UTF-8
  keyboard:
    layout: us
  network:
    network:
      version: 2
      ethernets:
        any:
          match:
            name: "en*"
          dhcp4: true
  storage:
    layout:
      name: direct
      sizing-policy: all
  packages:
    - openssh-server
{{- range .Packages}}
    - {{.}}
{{- end}}
  late-commands:
    - curtin in-target -- systemctl enable ssh
{{- range .Services}}
    - curtin in-target -- systemctl enable {{.}}
{{- end}}
{{- if .InstallUpdates}}
    - curtin in-target -- apt-get upgrade -y
{{- end}}
    - curtin in-target -- rm -f /etc/machine-id
    - curtin in-target -- touch /etc/machine-id
    - curtin in-target -- rm -f /etc/ssh/ssh_host_*
    - curtin in-target -- rm -f /etc/NetworkManager/system-connections/*
  shutdown: reboot
`))

func generateUbuntuAutoInstall(data templateData, opts BuildOptions) (*AutoInstallConfig, error) {
	packages, services := MergeRoles(opts.RoleIDs, DistroUbuntu)

	ud := ubuntuTemplateData{
		templateData:   data,
		Packages:       packages,
		Services:       services,
		InstallUpdates: opts.InstallUpdates,
	}

	var udBuf bytes.Buffer
	if err := ubuntuUserDataTemplate.Execute(&udBuf, ud); err != nil {
		return nil, fmt.Errorf("render ubuntu user-data: %w", err)
	}
	return &AutoInstallConfig{
		Format:           FormatAutoInstall,
		KickstartContent: udBuf.String(),
		MetaDataContent:  "instance-id: clonr-build\nlocal-hostname: generic\n",
		ISOLabel:         "CIDATA",
	}, nil
}

// ── Debian (preseed) ─────────────────────────────────────────────────────────

var debianPreseedTemplate = template.Must(template.New("debian-preseed").Parse(`# clonr auto-generated Debian preseed
d-i debian-installer/locale string en_US.UTF-8
d-i keyboard-configuration/xkb-keymap select us
d-i netcfg/choose_interface select auto
d-i netcfg/get_hostname string generic
d-i netcfg/get_domain string localdomain
d-i mirror/country string manual
d-i mirror/http/hostname string deb.debian.org
d-i mirror/http/directory string /debian
d-i mirror/http/proxy string
d-i passwd/root-password-crypted password {{.RootPasswordHash}}
d-i passwd/make-user boolean false
d-i clock-setup/utc boolean true
d-i time/zone string UTC
d-i partman-auto/disk string /dev/sda
d-i partman-auto/method string regular
d-i partman-auto/choose_recipe select atomic
d-i partman-partitioning/confirm_write_new_label boolean true
d-i partman/choose_partition select finish
d-i partman/confirm boolean true
d-i partman/confirm_nooverwrite boolean true
d-i base-system/install-recommends boolean false
d-i apt-setup/non-free boolean false
d-i apt-setup/contrib boolean false
tasksel tasksel/first multiselect standard
d-i pkgsel/include string openssh-server
d-i pkgsel/upgrade select none
d-i grub-installer/only_debian boolean true
d-i grub-installer/with_other_os boolean true
d-i grub-installer/bootdev string /dev/sda
d-i finish-install/reboot_in_progress note
d-i preseed/late_command string \
  in-target systemctl enable ssh; \
  in-target rm -f /etc/machine-id; \
  in-target touch /etc/machine-id; \
  in-target rm -f /etc/ssh/ssh_host_*
`))

func generateDebianPreseed(data templateData) (*AutoInstallConfig, error) {
	var buf bytes.Buffer
	if err := debianPreseedTemplate.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("render debian preseed: %w", err)
	}
	return &AutoInstallConfig{
		Format:           FormatPreseed,
		KickstartContent: buf.String(),
		ISOLabel:         "OEMDRV",
	}, nil
}

// ── SUSE / openSUSE (AutoYaST) ───────────────────────────────────────────────

var autoYaSTTemplate = template.Must(template.New("autoyast").Parse(`<?xml version="1.0"?>
<!DOCTYPE profile>
<profile xmlns="http://www.suse.com/1.0/yast2ns" xmlns:config="http://www.suse.com/1.0/configns">
  <general>
    <mode><confirm config:type="boolean">false</confirm></mode>
  </general>
  <language><language>en_US</language><languages/></language>
  <keyboard><keymap>english-us</keymap></keyboard>
  <timezone><hwclock>UTC</hwclock><timezone>UTC</timezone></timezone>
  <networking>
    <keep_install_network config:type="boolean">true</keep_install_network>
    <interfaces config:type="list">
      <interface>
        <bootproto>dhcp</bootproto>
        <name>eth0</name>
        <startmode>auto</startmode>
      </interface>
    </interfaces>
  </networking>
  <partitioning config:type="list">
    <drive>
      <device>/dev/sda</device>
      <disklabel>gpt</disklabel>
      <initialize config:type="boolean">true</initialize>
      <use>all</use>
      <partitions config:type="list">
        <partition>
          <partition_type>primary</partition_type>
          <filesystem config:type="symbol">btrfs</filesystem>
          <mount>/</mount>
          <size>max</size>
        </partition>
      </partitions>
    </drive>
  </partitioning>
  <software>
    <packages config:type="list">
      <package>openssh</package>
    </packages>
    <patterns config:type="list">
      <pattern>base</pattern>
    </patterns>
  </software>
  <users config:type="list">
    <user>
      <username>root</username>
      <user_password>{{.RootPasswordHash}}</user_password>
      <encrypted config:type="boolean">true</encrypted>
    </user>
  </users>
  <scripts>
    <post-scripts config:type="list">
      <script>
        <filename>clonr-scrub.sh</filename>
        <interpreter>shell</interpreter>
        <source><![CDATA[
#!/bin/bash
systemctl enable sshd
rm -f /etc/machine-id && touch /etc/machine-id
rm -f /etc/ssh/ssh_host_*
]]></source>
      </script>
    </post-scripts>
  </scripts>
</profile>
`))

func generateAutoYaST(data templateData) (*AutoInstallConfig, error) {
	var buf bytes.Buffer
	if err := autoYaSTTemplate.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("render autoyast: %w", err)
	}
	return &AutoInstallConfig{
		Format:           FormatAutoYaST,
		KickstartContent: buf.String(),
		ISOLabel:         "OEMDRV",
	}, nil
}

// ── Alpine (answers file) ─────────────────────────────────────────────────────

var alpineAnswersTemplate = template.Must(template.New("alpine").Parse(`KEYMAPOPTS="us us"
HOSTNAMEOPTS="-n generic"
INTERFACESOPTS="auto lo
iface lo inet loopback

auto eth0
iface eth0 inet dhcp
"
DNSOPTS="-d localdomain 8.8.8.8"
TIMEZONEOPTS="-z UTC"
PROXYOPTS="none"
APKREPOSOPTS="-1"
SSHDOPTS="-c openssh"
NTPOPTS="-c chrony"
DISKOPTS="-m sys /dev/sda"
`))

func generateAlpineAnswers(data templateData) (*AutoInstallConfig, error) {
	var buf bytes.Buffer
	if err := alpineAnswersTemplate.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("render alpine answers: %w", err)
	}
	return &AutoInstallConfig{
		Format:           FormatAnswers,
		KickstartContent: buf.String(),
		ISOLabel:         "OEMDRV",
	}, nil
}

// ensure joinStrings is used to satisfy the compiler — the template FuncMap
// closure captures strings.Join directly, so this just avoids the import warning.
var _ = joinStrings
