#!/bin/bash


fio --filename=/data/testing --direct=1 --rw=randrw --bs=4k --ioengine=libaio --iodepth=256 --numjobs=4 --time_based --group_reporting --name=rwtest --runtime=120 --eta-newline=1 
