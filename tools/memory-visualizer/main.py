import argparse
import os
import pandas as pd
import plotly.graph_objects as go
from plotly.subplots import make_subplots

# The CVS file should have the following columns:
# time, memory_usage, red_dot
# red_dot should be true or false
# The red_dot column is used to mark the points in the graph: those with red_dot=true should have
# red round markers, the others should have no markers

has_red_dot = False

# Get file from the argument
def main():
    global has_red_dot
    args = parse_args()
    if args.heap_file is None or args.rss_file is None:
        print("Please provide a heap file and a rss file")
        return
    # Check if file exists
    if not os.path.isfile(args.heap_file):
        print(f"Heap file '{args.heap_file}' does not exist")
        return
    if not os.path.isfile(args.rss_file):
        print(f"RSS file '{args.rss_file}' does not exist")
        return

    df_heap = pd.read_csv(args.heap_file)
    df_rss = pd.read_csv(args.rss_file)

    if 'red' in df_heap.columns:
        has_red_dot = True

    fig = make_subplots(rows=1, cols=1)

    # Add the main line with all the usage data
    fig.add_trace(go.Scatter(
        x=df_heap['time'],
        y=df_heap['memory_usage'],
        mode='lines',
        name='Memory Usage',
        line=dict(color='lightblue', width=2)
    ), row=1, col=1)

    # If the data has a red_dot column, add the red circles around the points with red_dot=true
    if has_red_dot:
        # Add the red circles around the points with red_dot=true
        red_dot = df_heap[df_heap['red'] == 1]
        fig.add_trace(go.Scatter(
            x=red_dot['time'],
            y=red_dot['memory_usage'],
            mode='markers',
            marker=dict(color='red', symbol='circle', size=2),
            name='Detector Triggered'), row=1, col=1)

        # Add the main line with RSS data from the RSS file
        fig.add_trace(go.Scatter(
        x=df_rss['time'],
        y=df_rss['memory_usage'],
        mode='lines',
        name='RSS',
        line=dict(color='lightgreen', width=2)
        ), row=1, col=1)

    def update_smoothed_data(window_size):
        df_heap['smoothed'] = df_heap['memory_usage'].rolling(window=window_size,
                                                              center=True).median()
        df_rss['smoothed'] = df_rss['memory_usage'].rolling(window=window_size,
                                                            center=True).median()
        return [df_heap['smoothed'], df_rss['smoothed']]

    # Initialize smoothed data with window size 10
    smoothed_data = update_smoothed_data(10)

    fig.add_trace(go.Scatter(
        x=df_heap['time'],
        y=smoothed_data[0],
        mode='lines',
        name='Smoothed Heap',
        line=dict(color='black', width=1)
    ), row=1, col=1)
    fig.add_trace(go.Scatter(
        x=df_rss['time'],
        y=smoothed_data[1],
        mode='lines',
        name='Smoothed RSS',
        line=dict(color='black', width=1)
    ), row=1, col=1)

    # Add buttons to switch between Bytes and Megabytes
    fig.update_layout(
        updatemenus=[
            dict(
                buttons=list([
                    dict(
                        args=[{'y': [df_heap['memory_usage'],
                                     red_dot['memory_usage'] if has_red_dot else None,
                                     df_rss['memory_usage']]}],
                        label='Bytes',
                        method='restyle'
                    ),
                    dict(
                        args=[{'y': [df_heap['memory_usage'] / 1024 / 1024,
                                     red_dot['memory_usage'] / 1024 / 1024 if has_red_dot else None,
                                     df_rss['memory_usage'] / 1024 / 1024],
                               }],
                        label='Megabytes',
                        method='restyle'
                    ),
                ]),
                direction='down',
                showactive=True,
            ),
        ],
        sliders=[{
            'steps': [
                {'method': 'restyle', 'label' : str(i), 'args': [{'y': update_smoothed_data(i)}, [3, 4]]}
                for i in range(10, 200, 20)
            ],
            'currentvalue': {'prefix': 'Window size: '},
        }],
    )

    fig.show()

def parse_args():
    parser = argparse.ArgumentParser(description='Visualize memory usage')
    parser.add_argument('heap_file', help='Pillar heap file to visualize')
    parser.add_argument('rss_file', help='RSS file to visualize')
    return parser.parse_args()


if __name__ == '__main__':
    main()
