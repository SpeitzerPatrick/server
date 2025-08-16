#!/bin/bash

# 备份原文件
cp docker-compose.yaml docker-compose.yaml.backup

# 使用sed注释掉所有loki日志配置
sed -i '/^[[:space:]]*logging:/,/^[[:space:]]*loki-batch-size:/ {
    s/^/# /
}' docker-compose.yaml

echo "已注释掉所有loki日志配置"
