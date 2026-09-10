# Home's own etcd backup bucket. Previously created by tofu/cloud (Hetzner
# Object Storage has no home-side Proxmox equivalent, so it lived wherever
# the AWS provider was already configured) — moved here because it's home's
# data, not cloud's, and home now has its own AWS-provider access anyway (see
# providers.tf) for its own state bucket below.
resource "random_id" "home_talos_backup_bucket" {
  byte_length = 4
}

resource "aws_s3_bucket" "home_talos_backups" {
  bucket        = "home-talos-etcd-${random_id.home_talos_backup_bucket.hex}"
  force_destroy = false

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_versioning" "home_talos_backups" {
  bucket = aws_s3_bucket.home_talos_backups.id
  versioning_configuration {
    status = "Enabled"
  }
}

# NOT managed here: aws_s3_bucket_lifecycle_configuration against Hetzner
# Object Storage never converges with the AWS provider (tested live —
# every apply times out after 3m on the post-write verification GET,
# "context deadline exceeded"). Confirmed upstream bug:
# terraform-provider-aws#49019 / still-open fix PR #49547 — the resource's
# read-back verification depends on an AWS-only response header
# (x-amz-transition-default-minimum-object-size) that Hetzner's
# S3-compatible API doesn't send. Snapshots accumulate forever without a
# lifecycle policy; set one manually (Hetzner Console, or
# `aws s3api put-bucket-lifecycle-configuration`) until the upstream fix
# ships.

output "home_talos_backup_bucket" {
  description = "Hetzner Object Storage bucket for Home Talos etcd snapshots"
  value       = aws_s3_bucket.home_talos_backups.bucket
}
