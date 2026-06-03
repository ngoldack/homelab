# Hetzner Object Storage (S3-compatible) bucket for offsite disaster-recovery.
#
# This single bucket holds every offsite backup stream, separated by object key
# prefixes (the in-cluster MinIO tenant remains the primary S3 for app data):
#   k8s-backups/   -> Velero            (kubernetes/infrastructure/controllers/backup)
#   etcd-backups/  -> talos-backup      (Talos etcd snapshots)
#   cnpg/<app>/    -> CloudNativePG Barman Cloud (authentik, n8n, khoj)
#
# Hetzner Object Storage has no management API, so the access/secret keys must be
# generated once by hand in the Hetzner Cloud Console and stored in secret.sops.yaml.
# This Terraform only creates/owns the bucket and its versioning + lifecycle config.

resource "aws_s3_bucket" "dr" {
  bucket = var.hetzner_dr_bucket_name

  # The bucket holds disaster-recovery data — never let `tofu destroy` nuke it.
  lifecycle {
    prevent_destroy = true
  }
}

# Versioning protects against accidental/ransomware overwrite of backups.
resource "aws_s3_bucket_versioning" "dr" {
  bucket = aws_s3_bucket.dr.id
  versioning_configuration {
    status = "Enabled"
  }
}

# Lifecycle rules: prune the streams that have no application-side retention.
# Velero (10d ttl) and Barman (14d retentionPolicy) prune their own objects, so
# we only need server-side expiry for the talos etcd snapshots, plus housekeeping
# for stale object versions and aborted multipart uploads across the whole bucket.
resource "aws_s3_bucket_lifecycle_configuration" "dr" {
  bucket = aws_s3_bucket.dr.id

  # talos-backup never prunes; expire etcd snapshots after 30 days.
  rule {
    id     = "expire-etcd-backups"
    status = "Enabled"
    filter {
      prefix = "etcd-backups/"
    }
    expiration {
      days = 30
    }
  }

  # Reclaim space from non-current versions left behind by versioning.
  rule {
    id     = "expire-noncurrent-versions"
    status = "Enabled"
    filter {
      prefix = ""
    }
    noncurrent_version_expiration {
      noncurrent_days = 30
    }
  }

  # Clean up failed multipart uploads so they don't accrue storage cost.
  rule {
    id     = "abort-incomplete-multipart"
    status = "Enabled"
    filter {
      prefix = ""
    }
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }
}
