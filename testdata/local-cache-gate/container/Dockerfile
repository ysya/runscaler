FROM ghcr.io/falcondev-oss/actions-runner@sha256:462ebdbc95fabf0bdf264ffe02ff5182665c6d99ac0f870b67d7f649c1ef1746
COPY index.js /probe/index.js
COPY post-cache.js /probe/post-cache.js
COPY --chmod=755 post.sh /usr/local/bin/post.sh
ENTRYPOINT ["/home/runner/externals/node24/bin/node", "/probe/index.js"]
