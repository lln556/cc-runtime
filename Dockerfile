FROM ubuntu:26.04
ENV DEBIAN_FRONTEND=noninteractive

# 开发机只跑 Claude Code，Bash 命令经 remote-shell 在 Mac 上执行，语言工具链都在 Mac 上。
# 镜像里只留 Claude Code 自己、SSH 登录和终端要用的东西。
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl \
    # 下载 Claude Code、Herdr、zim 的安装脚本
    ca-certificates \
    # HTTPS 证书，供 curl 和 git 使用
    sudo \
    # 让 dev 用户提权，入口脚本用它设置 SSH、machine-id 和 Claude Code 的系统级设置
    openssh-server \
    # SSH 远程登录
    locales \
    # 生成 UTF-8 locale，vim、less 等才能正确处理中文
    procps \
    # ps、kill 等进程工具
    less \
    # 分页查看，git 默认用它
    git \
    # Claude Code 在容器里调用 git，zim 安装模块也用它
    ripgrep \
    # rg，Claude Code 的 Grep 工具使用
    jq \
    # JSON 处理，Claude Code 的状态栏脚本使用
    python3 \
    # Claude Code 的状态栏脚本使用
    vim \
    # SSH 登录后编辑文件
    zsh \
    # 默认 Shell
    fzf \
    # 模糊查找，Ctrl-R 搜历史、Ctrl-T 搜文件，zoxide 的 zi 也用它
    zoxide \
    # 按使用频率跳转目录，命令 z / zi
    && apt-get clean && rm -rf /var/lib/apt/lists/*

# 生成 UTF-8 locale 并设为全局默认，否则 vim 等程序回退到 C locale，中文显示乱码。
RUN locale-gen en_US.UTF-8 && update-locale LANG=en_US.UTF-8
ENV LANG=en_US.UTF-8 \
    LC_ALL=en_US.UTF-8

# 安装 Node.js 24 LTS，Herdr 插件 herdr-auto-tab-name 用它直接运行 .mts。
# Ubuntu 源里的 nodejs 编译时没带 TypeScript 类型剥离，所以用 NodeSource 的官方构建。
RUN curl -fsSL https://deb.nodesource.com/setup_24.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && apt-get clean && rm -rf /var/lib/apt/lists/*

# 安装 Herdr，二进制放到 /usr/local/bin
RUN curl -fsSL https://herdr.dev/install.sh | env HERDR_INSTALL_DIR=/usr/local/bin sh

# 创建 dev 用户，默认 Shell 为 zsh
RUN useradd -m -s /bin/zsh dev && \
    usermod -aG sudo dev && \
    echo "dev ALL=(ALL) NOPASSWD:ALL" >> /etc/sudoers

# 配置 SSH
RUN sed -i 's/^#\\?PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config \
    && rm -f /etc/ssh/ssh_host_*

# 添加系统级别名
RUN echo 'alias claude="claude --dangerously-skip-permissions"' >> /etc/bash.bashrc

# 不在镜像中固化机器标识：软件包安装时生成的 machine-id 会被所有容器共用。
# 部署时每台开发机挂载自己的 machine-id；未挂载时由 entrypoint 首次启动生成。
RUN rm -f /var/lib/dbus/machine-id && truncate -s 0 /etc/machine-id

USER dev
WORKDIR /home/dev/workspace
ENV PATH="/home/dev/.local/bin:${PATH}"

# 用户级工具必须在 USER dev 之后安装，否则会落到 /root 下。
# 校验后删除运行 claude 留下的状态（含 userID / machineID），避免所有容器共用同一身份。
RUN curl -fsSL https://claude.ai/install.sh | bash \
    && /home/dev/.local/bin/claude --version \
    && rm -rf /home/dev/.claude /home/dev/.claude.json

# 本地文件放在工具安装之后，修改时复用 Claude Code 等工具层缓存。
# Claude Code 的系统级设置：shell 指向 remote-shell、SessionStart hook、CLAUDE.md。
COPY claude-code/ /etc/claude-code/
COPY --chmod=0755 scripts/entrypoint.sh /usr/local/bin/entrypoint.sh
COPY shell/ /opt/cc-runtime/
COPY --chmod=0755 shell/setup-shell.sh /opt/cc-runtime/setup-shell.sh

# 安装 zim，并写入 .zshenv / .zshrc / .zimrc
RUN /opt/cc-runtime/setup-shell.sh

# bash 仍保留 Claude Code 的 PATH 和别名
RUN echo 'export PATH="$HOME/.local/bin:$PATH"' >> /home/dev/.bashrc && \
    echo 'alias claude="claude --dangerously-skip-permissions"' >> /home/dev/.bashrc

EXPOSE 22

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
