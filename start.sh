#!/bin/bash

nohup ./bin/kf-ci -c prod/config.yaml 2>&1 > ci.log &