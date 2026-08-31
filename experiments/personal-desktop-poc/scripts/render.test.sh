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

# The Gateway derives the same bearer from OWNER_HASH_KEY; assert the render
# script embeds exactly that value in the VM's cloud-init.
EXPECTED_AGENT_TOKEN=$(printf 'desktop-agent-v1\nhttps://issuer.example\nsubject-123\ntrue\n' \
  | openssl dgst -sha256 -hmac 'kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk' -binary \
  | od -An -tx1 \
  | tr -d ' \n') \
ruby -ryaml - "$platform_output" "$desktop_output" <<'RUBY'
platform = YAML.load_stream(File.read(ARGV.fetch(0)))
desktop = YAML.load_stream(File.read(ARGV.fetch(1)))

deployment = platform.find { |document| document["kind"] == "Deployment" }
golden = platform.find { |document| document["kind"] == "DataVolume" }
raise "rendered platform document count changed" unless platform.length == 7
raise "rendered desktop document count changed" unless desktop.length == 3

service = desktop.find { |document| document["kind"] == "Service" }
data_volume = desktop.find { |document| document["kind"] == "DataVolume" }
virtual_machine = desktop.find { |document| document["kind"] == "VirtualMachine" }

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

desktop_strings = [
  data_volume.dig("metadata", "name"),
  data_volume.dig("metadata", "namespace"),
  data_volume.dig("spec", "source", "pvc", "name"),
  data_volume.dig("spec", "source", "pvc", "namespace"),
  data_volume.dig("spec", "storage", "resources", "requests", "storage"),
  data_volume.dig("spec", "storage", "storageClassName"),
  virtual_machine.dig("metadata", "name"),
  virtual_machine.dig("metadata", "namespace"),
  service.dig("metadata", "name"),
  service.dig("metadata", "namespace"),
  virtual_machine.dig("spec", "template", "spec", "nodeSelector", "kubernetes.io/hostname"),
  virtual_machine.dig("spec", "template", "spec", "domain", "resources", "requests", "memory"),
]

raise "platform placeholder rendered as a non-string YAML scalar" unless platform_strings.all? { |value| value.is_a?(String) }
raise "desktop placeholder rendered as a non-string YAML scalar" unless desktop_strings.all? { |value| value.is_a?(String) }
raise "CPU cores must remain an integer" unless virtual_machine.dig("spec", "template", "spec", "domain", "cpu", "cores").is_a?(Integer)
raise "cloud-init hostname is not quoted" unless virtual_machine.dig("spec", "template", "spec", "volumes", 1, "cloudInitNoCloud", "userData").include?('hostname: "pd-')
raise "cloud-init SSH key is missing" unless virtual_machine.dig("spec", "template", "spec", "volumes", 1, "cloudInitNoCloud", "userData").include?('ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestOnlyKey typeclaw-desktop-poc')
raise "desktop-agent Service missing" unless service
raise "agent Service name wrong" unless service.dig("metadata", "name") == "#{virtual_machine.dig('metadata', 'name')}-agent"
raise "agent Service port wrong" unless service.dig("spec", "ports", 0, "port") == 9876
raise "agent Service selector wrong" unless service.dig("spec", "selector", "kubevirt.io/domain") == virtual_machine.dig("metadata", "name")
raise "VM masquerade agent port not declared" unless virtual_machine.dig("spec", "template", "spec", "domain", "devices", "interfaces", 0, "ports", 0, "port") == 9876
user_data = virtual_machine.dig("spec", "template", "spec", "volumes", 1, "cloudInitNoCloud", "userData")
raise "desktop-agent script not inlined" unless user_data.include?("/usr/local/bin/desktop-agent.py") && user_data.include?("desktop-agent/1.0")
raise "desktop-agent render placeholder survived" if user_data.include?("__DESKTOP_AGENT_PYTHON__")
raise "agent autostart entry missing" unless user_data.include?("Exec=/usr/local/bin/desktop-agent.py")
raise "desktop-agent token missing or wrong" unless user_data.include?(ENV.fetch("EXPECTED_AGENT_TOKEN"))
raise "desktop-agent token path missing" unless user_data.include?("/etc/personal-desktop/agent-token")
raise "desktop-agent tool packages missing" unless user_data.include?("xdotool") && user_data.include?("scrot") && user_data.include?("wmctrl")
RUBY
