FROM node:18-bookworm-slim AS contracts-builder
RUN apt-get update && \
    apt-get install -y git python3 make g++ curl software-properties-common gnupg
# Install Rust
RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --default-toolchain 1.86.0
ENV PATH="/root/.cargo/bin:${PATH}"
# Install Foundry pin to 1.0.0
RUN curl -L https://foundry.paradigm.xyz | bash && . ~/.bashrc && ~/.foundry/bin/foundryup -C 8692e926198056d0228c1e166b1b6c34a5bed66
WORKDIR /workspace
COPY contracts-legacy/package.json contracts-legacy/yarn.lock contracts-legacy/
RUN cd contracts-legacy && yarn install
COPY contracts/package.json contracts/yarn.lock contracts/
RUN cd contracts && yarn install
COPY contracts-legacy contracts-legacy/
COPY contracts contracts/
COPY safe-smart-account safe-smart-account/
RUN cd safe-smart-account && yarn install
COPY Makefile .
RUN . ~/.bashrc && NITRO_BUILD_IGNORE_TIMESTAMPS=1 make build-solidity

