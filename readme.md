# MegaRAID exporter

Prometheus Exporter for RAID controller status

Can be used with storcli/perrcli


# Usage

./megaraid-exporter -storcli=/path/to/cli/utility

Arguments:

- `-storcli` path to command line utility
- `-web.listen-address`: interface:port where to expose the metrics (default 127.0.0.1:9333)
- `-web.telemetry-path`: path on which expose metrics (default '/metrics')
- `-controller` : controller to select (default 'all')
- `-disk` : Disk to select (default 'all')

# Disclaimer

This project is based on a rewrite from https://github.com/sentiweb/collectd-megaraid using Claude AI (Sonnet (5.5)
From the file https://github.com/sentiweb/collectd-megaraid/blob/main/collectd_megaraid/collector.py at commit 0932981235150afd290cca6620a145462bafd71e

