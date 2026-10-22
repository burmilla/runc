# libcontainer packages for System Docker

Trimmed fork of runc 1.0.0-rc3 (Docker 17.06 era) that only keeps the
`libcontainer/configs`, `devices`, `system`, `user` and `utils` packages
vendored by [burmilla/docker](https://github.com/burmilla/docker) (System
Docker for BurmillaOS). The runc binary itself is no longer built here;
System Docker runs containers with upstream [crun](https://github.com/containers/crun).
