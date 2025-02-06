FROM prom/node-exporter:latest

CMD ["--path.procfs=/hostfs/proc", "--path.sysfs=/hostfs/sys", "--path.rootfs=/hostfs", "--collector.cgroups", "--collector.processes"]
