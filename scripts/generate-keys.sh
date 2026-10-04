#!/usr/bin/env bash
# generate-keys.sh
# 生成 JWT 签名密钥对（支持 RS256 和 ES256）
# 输出目录：server/etc/keys/
#
# 用法：
#   ./scripts/generate-keys.sh [rs256|es256|all]
#
# 示例：
#   ./scripts/generate-keys.sh all        # 生成 RS256 和 ES256
#   ./scripts/generate-keys.sh rs256      # 仅生成 RS256
#   ./scripts/generate-keys.sh es256      # 仅生成 ES256

set -euo pipefail

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# 输出目录
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KEYS_DIR="${SCRIPT_DIR}/../etc/keys"

# 确保输出目录存在
mkdir -p "${KEYS_DIR}"

# 检查依赖
check_dependency() {
    if ! command -v openssl &> /dev/null; then
        echo -e "${RED}错误: 未找到 openssl，请先安装${NC}"
        exit 1
    fi
}

# 生成 RS256 密钥对（RSA 2048）
generate_rs256() {
    echo -e "${GREEN}生成 RS256 密钥对...${NC}"

    local key_file="${KEYS_DIR}/rs256-private.pem"
    local pub_file="${KEYS_DIR}/rs256-public.pem"

    openssl genrsa -out "${key_file}" 2048 2>/dev/null
    openssl rsa -in "${key_file}" -pubout -out "${pub_file}" 2>/dev/null

    chmod 600 "${key_file}"
    chmod 644 "${pub_file}"

    echo -e "${GREEN}  私钥: ${key_file} (权限 600)${NC}"
    echo -e "${GREEN}  公钥: ${pub_file} (权限 644)${NC}"
}

# 生成 ES256 密钥对（ECDSA P-256）
generate_es256() {
    echo -e "${GREEN}生成 ES256 密钥对...${NC}"

    local key_file="${KEYS_DIR}/es256-private.pem"
    local pub_file="${KEYS_DIR}/es256-public.pem"

    openssl ecparam -genkey -name prime256v1 -noout -out "${key_file}" 2>/dev/null
    openssl ec -in "${key_file}" -pubout -out "${pub_file}" 2>/dev/null

    chmod 600 "${key_file}"
    chmod 644 "${pub_file}"

    echo -e "${GREEN}  私钥: ${key_file} (权限 600)${NC}"
    echo -e "${GREEN}  公钥: ${pub_file} (权限 644)${NC}"
}

# 显示使用说明
show_usage() {
    echo ""
    echo -e "${YELLOW}使用说明:${NC}"
    echo "  在 Go 服务配置中引用生成的密钥："
    echo ""
    echo "  jwt:"
    echo "    algorithm: RS256  # 或 ES256"
    echo "    private_key_path: etc/keys/rs256-private.pem"
    echo "    public_key_path:  etc/keys/rs256-public.pem"
    echo ""
}

# ============================================================
# 主流程
# ============================================================

check_dependency

ACTION="${1:-all}"

case "${ACTION}" in
    rs256)
        generate_rs256
        show_usage
        ;;
    es256)
        generate_es256
        show_usage
        ;;
    all)
        generate_rs256
        echo ""
        generate_es256
        show_usage
        ;;
    *)
        echo -e "${RED}错误: 未知参数 '${ACTION}'${NC}"
        echo "用法: $0 [rs256|es256|all]"
        exit 1
        ;;
esac

echo ""
echo -e "${GREEN}密钥生成完成！${NC}"
echo -e "${YELLOW}请妥善保管私钥文件，切勿提交到版本控制系统！${NC}"
