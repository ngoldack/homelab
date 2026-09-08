variable "state_encryption_passphrase" {
  type      = string
  sensitive = true
  nullable  = false
}

variable "cluster_name" {
  type    = string
  default = "cloud-talos"
}

variable "cluster_domain" {
  type    = string
  default = "cloud.cluster.local"
}

variable "location" {
  type    = string
  default = "fsn1"
}

variable "server_type" {
  type    = string
  default = "cax11"
}

variable "object_storage_location" {
  type        = string
  default     = "fsn1"
  description = "Hetzner Object Storage location for Talos etcd backups"
}

variable "talos_version" {
  type    = string
  default = "v1.13.4"
}

variable "kubernetes_version" {
  type    = string
  default = "1.36.2"
}