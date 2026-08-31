#!/bin/sh
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
platform_output=$(mktemp)
desktop_output=$(mktemp)
trap 'rm -f -- "$platform_output" "$desktop_output"' EXIT HUP INT TERM

DESKTOP_NAMESPACE=true \
GATEWAY_IMAGE=true \
TYPECLAW_INSTANCE_UID=true \
GOLDEN_IMAGE_NAME=true \
GOLDEN_IMAGE_URL=true \
GOLDEN_DISK_SIZE=1 \
STORAGE_CLASS=true \
sh "$script_dir/render-platform.sh" >"$platform_output"

OWNER_ISSUER=https://issuer.example \
OWNER_SUBJECT=subject-123 \
TYPECLAW_INSTANCE_UID=true \
OWNER_HASH_KEY=kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk \
DESKTOP_NAMESPACE=true \
GOLDEN_IMAGE_NAME=true \
STORAGE_CLASS=true \
DESKTOP_DISK_SIZE=1 \
DESKTOP_MEMORY=1 \
DESKTOP_CPU_CORES=4 \
DESKTOP_NODE_NAME=k12-01 \
DESKTOP_SSH_AUTHORIZED_KEY='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestOnlyKey typeclaw-desktop-poc' \
sh "$script_dir/render-personal-desktop.sh" >"$desktop_output"

ruby -ryaml - "$platform_output" "$desktop_output" <<'RUBY'
platform = YAML.load_stream(File.read(ARGV.fetch(0)))
desktop = YAML.load_stream(File.read(ARGV.fetch(1)))

deployment = platform.find { |document| document["kind"] == "Deployment" }
golden = platform.find { |document| document["kind"] == "DataVolume" }
raise "rendered platform document count changed" unless platform.length == 7
raise "rendered desktop document count changed" unless desktop.length == 2

platform_strings = platform.flat_map do |document|
  metadata = document.fetch("metadata")
  [metadata["name"], metadata["namespace"]].compact
end
platform_strings.concat([
  deployment.dig("spec", "template", "spec", "containers", 0, "image"),
  deployment.dig("spec", "template", "spec", "containers", 0, "env", 0, "value"),
  deployment.dig("spec", "template", "spec", "containers", 0, "env", 1, "value"),
  golden.dig("spec", "source", "http", "url"),
  golden.dig("spec", "storage", "resources", "requests", "storage"),
  golden.dig("spec", "storage", "storageClassName"),
])

data_volume, virtual_machine = desktop
desktop_strings = [
  data_volume.dig("metadata", "name"),
  data_volume.dig("metadata", "namespace"),
  data_volume.dig("spec", "source", "pvc", "name"),
  data_volume.dig("spec", "source", "pvc", "namespace"),
  data_volume.dig("spec", "storage", "resources", "requests", "storage"),
  data_volume.dig("spec", "storage", "storageClassName"),
  virtual_machine.dig("metadata", "name"),
  virtual_machine.dig("metadata", "namespace"),
  virtual_machine.dig("spec", "template", "spec", "nodeSelector", "kubernetes.io/hostname"),
  virtual_machine.dig("spec", "template", "spec", "domain", "resources", "requests", "memory"),
]

raise "platform placeholder rendered as a non-string YAML scalar" unless platform_strings.all? { |value| value.is_a?(String) }
raise "desktop placeholder rendered as a non-string YAML scalar" unless desktop_strings.all? { |value| value.is_a?(String) }
raise "CPU cores must remain an integer" unless virtual_machine.dig("spec", "template", "spec", "domain", "cpu", "cores").is_a?(Integer)
raise "cloud-init hostname is not quoted" unless virtual_machine.dig("spec", "template", "spec", "volumes", 1, "cloudInitNoCloud", "userData").include?('hostname: "pd-')
raise "cloud-init SSH key is missing" unless virtual_machine.dig("spec", "template", "spec", "volumes", 1, "cloudInitNoCloud", "userData").include?('ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestOnlyKey typeclaw-desktop-poc')
RUBY
