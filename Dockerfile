FROM node:18-bookworm-slim AS contracts-builder
RUN apt-get update && \
    apt-get install -y git python3 make g++ curl
RUN curl -L https://foundry.paradigm.xyz | bash && . ~/.bashrc && ~/.foundry/bin/foundryup
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
